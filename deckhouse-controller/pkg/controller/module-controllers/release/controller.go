// Copyright 2023 Flant JSC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package release

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/flant/addon-operator/pkg/utils/logger"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	"github.com/deckhouse/deckhouse/deckhouse-controller/pkg/client/clientset/versioned"
	d8informers "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/client/informers/externalversions/deckhouse.io/v1alpha1"
	d8listers "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/client/listers/deckhouse.io/v1alpha1"
	"github.com/deckhouse/deckhouse/deckhouse-controller/pkg/controller/module-controllers/downloader"
	"github.com/deckhouse/deckhouse/deckhouse-controller/pkg/controller/module-controllers/utils"
	deckhouseconfig "github.com/deckhouse/deckhouse/go_lib/deckhouse-config"
)

// Controller is the controller implementation for ModuleRelease resources
type Controller struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface

	// d8ClientSet is a clientset for our own API group
	d8ClientSet versioned.Interface

	moduleReleasesLister d8listers.ModuleReleaseLister
	moduleSourcesLister  d8listers.ModuleSourceLister
	moduleReleasesSynced cache.InformerSynced
	moduleSourcesSynced  cache.InformerSynced

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface

	logger logger.Logger

	// <module-name>: <module-source>
	sourceModules map[string]string

	externalModulesDir string
	symlinksDir        string

	m             sync.Mutex
	delayTimer    *time.Timer
	restartReason string
}

// NewController returns a new sample controller
func NewController(ks kubernetes.Interface, d8ClientSet versioned.Interface, moduleReleaseInformer d8informers.ModuleReleaseInformer, moduleSourceInformer d8informers.ModuleSourceInformer) *Controller {
	ratelimiter := workqueue.NewMaxOfRateLimiter(
		workqueue.NewItemExponentialFailureRateLimiter(500*time.Millisecond, 1000*time.Second),
		&workqueue.BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(50), 300)},
	)

	lg := log.WithField("component", "ModuleReleaseController")

	controller := &Controller{
		kubeclientset:        ks,
		d8ClientSet:          d8ClientSet,
		moduleReleasesLister: moduleReleaseInformer.Lister(),
		moduleReleasesSynced: moduleReleaseInformer.Informer().HasSynced,
		moduleSourcesLister:  moduleSourceInformer.Lister(),
		moduleSourcesSynced:  moduleSourceInformer.Informer().HasSynced,
		workqueue:            workqueue.NewRateLimitingQueue(ratelimiter),
		logger:               lg,

		sourceModules: make(map[string]string),

		externalModulesDir: os.Getenv("EXTERNAL_MODULES_DIR"),
		symlinksDir:        filepath.Join(os.Getenv("EXTERNAL_MODULES_DIR"), "modules"),

		delayTimer: time.NewTimer(5 * time.Second),
	}

	// Set up an event handler for when ModuleSource resources change
	_, _ = moduleReleaseInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueModuleRelease,
		UpdateFunc: func(old, new interface{}) {
			newMS := new.(*v1alpha1.ModuleRelease)
			oldMS := old.(*v1alpha1.ModuleRelease)

			if newMS.ResourceVersion == oldMS.ResourceVersion {
				// Periodic resync will send update events for all known ModuleRelease.
				return
			}

			controller.enqueueModuleRelease(new)
		},
		DeleteFunc: controller.enqueueModuleRelease,
	})

	return controller
}

func (c *Controller) enqueueModuleRelease(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.logger.Debugf("enqueue ModuleRelease: %s", key)
	c.workqueue.Add(key)
}

func (c *Controller) emitRestart(msg string) {
	c.m.Lock()
	c.delayTimer.Reset(5 * time.Second)
	c.restartReason = msg
	c.m.Unlock()
}
func (c *Controller) restartLoop(ctx context.Context) {
	for {
		c.m.Lock()
		select {
		case <-c.delayTimer.C:
			if c.restartReason != "" {
				c.logger.Infof("Restarting Deckhouse because %s", c.restartReason)

				err := syscall.Kill(1, syscall.SIGUSR2)
				if err != nil {
					c.logger.Fatalf("Send SIGUSR2 signal failed: %s", err)
				}
			}
			c.delayTimer.Reset(5 * time.Second)

		case <-ctx.Done():
			return
		}

		c.m.Unlock()
	}
}

func (c *Controller) Run(ctx context.Context, workers int) {
	if c.externalModulesDir == "" {
		c.logger.Info("env: 'EXTERNAL_MODULES_DIR' is empty, we are not going to work with source modules")
		return
	}

	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	c.logger.Info("Starting ModuleRelease controller")

	// Wait for the caches to be synced before starting workers
	c.logger.Debug("Waiting for ModuleReleaseInformer caches to sync")

	go c.restartLoop(ctx)

	if ok := cache.WaitForCacheSync(ctx.Done(), c.moduleReleasesSynced); !ok {
		c.logger.Fatal("failed to wait for caches to sync")
	}

	c.logger.Infof("Starting workers count: %d", workers)
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	<-ctx.Done()
	c.logger.Info("Shutting down workers")
}

func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			c.logger.Errorf("expected string in workqueue but got %#v", obj)
			return nil
		}

		// run reconcile loop
		result, err := c.Reconcile(ctx, key)
		switch {
		case result.RequeueAfter != 0:
			c.workqueue.AddAfter(key, result.RequeueAfter)

		case result.Requeue:
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(key)

		default:
			c.workqueue.Forget(key)
		}

		return err
	}(obj)

	if err != nil {
		c.logger.Errorf("ModuleRelease reconcile error: %s", err.Error())
		return true
	}

	return true
}

const (
	fsReleaseFinalizer     = "modules.deckhouse.io/exist-on-fs"
	sourceReleaseFinalizer = "modules.deckhouse.io/release-exists"
)

// only ModuleRelease with active finalizer can get here, we have to remove the module on filesystem and remove the finalizer
func (c *Controller) deleteReconcile(ctx context.Context, roMR *v1alpha1.ModuleRelease) (ctrl.Result, error) {
	// deleted release
	// also cleanup the filesystem
	modulePath := path.Join(c.externalModulesDir, roMR.Spec.ModuleName, "v"+roMR.Spec.Version.String())

	err := os.RemoveAll(modulePath)
	if err != nil {
		return ctrl.Result{Requeue: true}, err
	}

	if roMR.Status.Phase == v1alpha1.PhaseDeployed {
		symlinkPath := filepath.Join(c.externalModulesDir, "modules", fmt.Sprintf("%d-%s", roMR.Spec.Weight, roMR.Spec.ModuleName))
		err := os.RemoveAll(symlinkPath)
		if err != nil {
			return ctrl.Result{Requeue: true}, err
		}
	}

	if !controllerutil.ContainsFinalizer(roMR, fsReleaseFinalizer) {
		return ctrl.Result{}, nil
	}

	mr := roMR.DeepCopy()
	controllerutil.RemoveFinalizer(mr, fsReleaseFinalizer)
	_, err = c.d8ClientSet.DeckhouseV1alpha1().ModuleReleases().Update(ctx, mr, metav1.UpdateOptions{})
	if err != nil {
		return ctrl.Result{Requeue: true}, err
	}

	return ctrl.Result{}, nil
}

func (c *Controller) createOrUpdateReconcile(ctx context.Context, roMR *v1alpha1.ModuleRelease) (ctrl.Result, error) {
	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use DeepCopy() to make a deep copy of original object and modify this copy
	// Or create a copy manually for better performance
	mr := roMR.DeepCopy()

	switch mr.Status.Phase {
	case "":
		mr.Status.Phase = v1alpha1.PhasePending
		if e := c.updateModuleReleaseStatus(ctx, mr); e != nil {
			return ctrl.Result{Requeue: true}, e
		}

		return ctrl.Result{}, nil

	case v1alpha1.PhaseSuperseded, v1alpha1.PhaseSuspended:
		// update labels
		addLabels(mr, map[string]string{"status": strings.ToLower(mr.Status.Phase)})
		if err := c.updateModuleRelease(ctx, mr); err != nil {
			return ctrl.Result{Requeue: true}, err
		}
		return ctrl.Result{}, nil

	case v1alpha1.PhaseDeployed:
		// add finalizer and status label
		if !controllerutil.ContainsFinalizer(mr, fsReleaseFinalizer) {
			controllerutil.AddFinalizer(mr, fsReleaseFinalizer)
		}

		addLabels(mr, map[string]string{"status": strings.ToLower(v1alpha1.PhaseDeployed)})
		if e := c.updateModuleRelease(ctx, mr); e != nil {
			return ctrl.Result{Requeue: true}, c.updateModuleRelease(ctx, mr)
		}

		// at least one release for module source is deployed, add finalizer to prevent module source deletion
		ms, err := c.moduleSourcesLister.Get(mr.GetModuleSource())
		if err != nil {
			return ctrl.Result{Requeue: true}, err
		}

		if !controllerutil.ContainsFinalizer(ms, sourceReleaseFinalizer) {
			ms = ms.DeepCopy()
			controllerutil.AddFinalizer(ms, sourceReleaseFinalizer)
			_, err = c.d8ClientSet.DeckhouseV1alpha1().ModuleSources().Update(ctx, ms, metav1.UpdateOptions{})
			if err != nil {
				return ctrl.Result{Requeue: true}, err
			}
		}

		return ctrl.Result{}, nil
	}

	// process only pending releases
	return c.reconcilePendingRelease(ctx, mr)
}

func (c *Controller) reconcilePendingRelease(ctx context.Context, mr *v1alpha1.ModuleRelease) (ctrl.Result, error) {
	moduleName := mr.Spec.ModuleName

	otherReleases, err := c.moduleReleasesLister.List(labels.SelectorFromValidatedSet(map[string]string{"module": moduleName}))
	if err != nil {
		return ctrl.Result{Requeue: true}, err
	}

	sort.Sort(byVersion(otherReleases))
	pred := newReleasePredictor(otherReleases)

	pred.calculateRelease()

	// search symlink for module by regexp
	// module weight for a new version of the module may be different from the old one,
	// we need to find a symlink that contains the module name without looking at the weight prefix.
	currentModuleSymlink, err := findExistingModuleSymlink(c.symlinksDir, moduleName)
	if err != nil {
		currentModuleSymlink = "900-" + moduleName // fallback
	}

	var modulesChangedReason string

	if pred.currentReleaseIndex == len(pred.releases)-1 {
		// latest release deployed
		deployedRelease := pred.releases[pred.currentReleaseIndex]
		deckhouseconfig.Service().AddModuleNameToSource(deployedRelease.Spec.ModuleName, deployedRelease.GetModuleSource())
		c.sourceModules[deployedRelease.Spec.ModuleName] = deployedRelease.GetModuleSource()

		// check symlink exists on FS, relative symlink
		modulePath := generateModulePath(moduleName, deployedRelease.Spec.Version.String())
		if !isModuleExistsOnFS(c.symlinksDir, currentModuleSymlink, modulePath) {
			newModuleSymlink := path.Join(c.symlinksDir, fmt.Sprintf("%d-%s", deployedRelease.Spec.Weight, moduleName))
			c.logger.Debugf("Module %q is not exists on the filesystem. Restoring", moduleName)
			err = enableModule(c.externalModulesDir, currentModuleSymlink, newModuleSymlink, modulePath)
			if err != nil {
				c.logger.Errorf("Module restore failed: %v", err)
				if e := c.suspendModuleVersionForRelease(ctx, deployedRelease, err); e != nil {
					return ctrl.Result{Requeue: true}, e
				}

				return ctrl.Result{Requeue: true}, err
			}
			modulesChangedReason = "one of modules is not enabled"
		}
	}

	if len(pred.skippedPatchesIndexes) > 0 {
		for _, index := range pred.skippedPatchesIndexes {
			release := pred.releases[index]
			release.Status.Phase = v1alpha1.PhaseSuperseded

			if e := c.updateModuleReleaseStatus(ctx, release); e != nil {
				return ctrl.Result{Requeue: true}, e
			}
		}
	}

	if pred.currentReleaseIndex >= 0 {
		release := pred.releases[pred.currentReleaseIndex]
		release.Status.Phase = v1alpha1.PhaseSuperseded
		release.Status.Message = ""

		if e := c.updateModuleReleaseStatus(ctx, release); e != nil {
			return ctrl.Result{Requeue: true}, e
		}
	}

	if pred.desiredReleaseIndex >= 0 {
		release := pred.releases[pred.desiredReleaseIndex]

		modulePath := generateModulePath(moduleName, release.Spec.Version.String())
		newModuleSymlink := path.Join(c.symlinksDir, fmt.Sprintf("%d-%s", release.Spec.Weight, moduleName))

		err := enableModule(c.externalModulesDir, currentModuleSymlink, newModuleSymlink, modulePath)
		if err != nil {
			c.logger.Errorf("Module deploy failed: %v", err)
			if e := c.suspendModuleVersionForRelease(ctx, release, err); e != nil {
				return ctrl.Result{Requeue: true}, e
			}
		}
		modulesChangedReason = "a new module release found"

		release.Status.Phase = v1alpha1.PhaseDeployed
		release.Status.Message = ""
		c.sendDocumentation(ctx, modulePath)
		if e := c.updateModuleReleaseStatus(ctx, release); e != nil {
			return ctrl.Result{Requeue: true}, e
		}
	}

	if modulesChangedReason != "" {
		c.emitRestart(modulesChangedReason)
	}

	return ctrl.Result{}, nil
}

// nolint: revive
func (c *Controller) sendDocumentation(ctx context.Context, _ string) {
	return
	// TODO: placeholder for documentation

	// nolint: govet
	list, err := c.kubeclientset.DiscoveryV1().EndpointSlices("d8-system").List(ctx, metav1.ListOptions{LabelSelector: "app=documentation"})
	if err != nil {
		// TODO: handle error
		panic(err)
	}

	for _, eps := range list.Items {
		var port int32
		for _, p := range eps.Ports {
			// TODO: find builder port
			if *p.Name == "???" {
				port = *p.Port
			}
		}

		if port == 0 {
			continue
		}
		for _, ep := range eps.Endpoints {
			for _, addr := range ep.Addresses {
				_, _ = http.DefaultClient.Post(fmt.Sprintf("http://%s:%d/???", addr, port), "TODO", nil)
			}
		}
	}
}

func (c *Controller) Reconcile(ctx context.Context, releaseName string) (ctrl.Result, error) {
	// Get the ModuleRelease resource with this name
	mr, err := c.moduleReleasesLister.Get(releaseName)
	if err != nil {
		// The ModuleRelease resource may no longer exist, in which case we stop
		// processing.
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{Requeue: true}, err
	}

	if !mr.DeletionTimestamp.IsZero() {
		return c.deleteReconcile(ctx, mr)
	}

	return c.createOrUpdateReconcile(ctx, mr)
}

func (c *Controller) suspendModuleVersionForRelease(ctx context.Context, release *v1alpha1.ModuleRelease, err error) error {
	if os.IsNotExist(err) {
		err = errors.New("not found")
	}

	release.Status.Phase = v1alpha1.PhaseSuspended
	release.Status.Message = fmt.Sprintf("Desired version of the module met problems: %s", err)

	return c.updateModuleReleaseStatus(ctx, release)
}

func enableModule(externalModulesDir, oldSymlinkPath, newSymlinkPath, modulePath string) error {
	if oldSymlinkPath != "" {
		if _, err := os.Lstat(oldSymlinkPath); err == nil {
			err = os.Remove(oldSymlinkPath)
			if err != nil {
				return err
			}
		}
	}

	if _, err := os.Lstat(newSymlinkPath); err == nil {
		err = os.Remove(newSymlinkPath)
		if err != nil {
			return err
		}
	}

	// make absolute path for versioned module
	moduleAbsPath := filepath.Join(externalModulesDir, strings.TrimPrefix(modulePath, "../"))
	// check that module exists on a disk
	if _, err := os.Stat(moduleAbsPath); os.IsNotExist(err) {
		return err
	}

	return os.Symlink(modulePath, newSymlinkPath)
}

func findExistingModuleSymlink(rootPath, moduleName string) (string, error) {
	var symlinkPath string

	moduleRegexp := regexp.MustCompile(`^(([0-9]+)-)?(` + moduleName + `)$`)
	walkDir := func(path string, d os.DirEntry, err error) error {
		if !moduleRegexp.MatchString(d.Name()) {
			return nil
		}

		symlinkPath = path
		return filepath.SkipDir
	}

	err := filepath.WalkDir(rootPath, walkDir)

	return symlinkPath, err
}

func generateModulePath(moduleName, version string) string {
	return path.Join("../", moduleName, "v"+version)
}

func isModuleExistsOnFS(symlinksDir, symlinkPath, modulePath string) bool {
	targetPath, err := filepath.EvalSymlinks(symlinkPath)
	if err != nil {
		return false
	}

	if filepath.IsAbs(targetPath) {
		targetPath, err = filepath.Rel(symlinksDir, targetPath)
		if err != nil {
			return false
		}
	}

	return targetPath == modulePath
}

func addLabels(mr *v1alpha1.ModuleRelease, labels map[string]string) {
	lb := mr.GetLabels()
	if len(lb) == 0 {
		mr.SetLabels(labels)
	} else {
		for l, v := range labels {
			lb[l] = v
		}
	}
}

func (c *Controller) updateModuleReleaseStatus(ctx context.Context, mrCopy *v1alpha1.ModuleRelease) error {
	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use DeepCopy() to make a deep copy of original object and modify this copy
	// Or create a copy manually for better performance
	mrCopy.Status.TransitionTime = metav1.NewTime(time.Now().UTC())
	_, err := c.d8ClientSet.DeckhouseV1alpha1().ModuleReleases().UpdateStatus(ctx, mrCopy, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	return nil
}

func (c *Controller) updateModuleRelease(ctx context.Context, mrCopy *v1alpha1.ModuleRelease) error {
	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use DeepCopy() to make a deep copy of original object and modify this copy
	// Or create a copy manually for better performance
	_, err := c.d8ClientSet.DeckhouseV1alpha1().ModuleReleases().Update(ctx, mrCopy, metav1.UpdateOptions{})
	return err
}

// RunPreflightCheck start a few checks and synchronize deckhouse filesystem with ModuleReleases
//   - Download modules, which have status=deployed on ModuleRelease but have no files on Filesystem
//   - Delete modules, that don't have ModuleRelease presented in the cluster
func (c *Controller) RunPreflightCheck(ctx context.Context) error {
	if c.externalModulesDir == "" {
		return nil
	}

	if ok := cache.WaitForCacheSync(ctx.Done(), c.moduleReleasesSynced, c.moduleSourcesSynced); !ok {
		c.logger.Fatal("failed to wait for caches to sync")
	}

	err := c.restoreAbsentSourceModules()
	if err != nil {
		return err
	}

	return c.deleteModulesWithAbsentRelease()
}

func (c *Controller) deleteModulesWithAbsentRelease() error {
	symlinksDir := filepath.Join(c.externalModulesDir, "modules")

	fsModulesLinks, err := c.readModulesFromFS(symlinksDir)
	if err != nil {
		return fmt.Errorf("read source modules from the filesystem failed: %w", err)
	}

	releases, err := c.moduleReleasesLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("fetch ModuleReleases failed: %w", err)
	}

	c.logger.Debugf("%d ModuleReleases found", len(releases))

	for _, release := range releases {
		c.sourceModules[release.Spec.ModuleName] = release.GetModuleSource()
		delete(fsModulesLinks, release.Spec.ModuleName)
	}

	if len(fsModulesLinks) > 0 {
		for module, moduleLinkPath := range fsModulesLinks {
			c.logger.Warnf("Module %q has no releases. Purging from FS", module)
			_ = os.RemoveAll(moduleLinkPath)
		}
	}

	return nil
}

func (c *Controller) GetModuleSources() map[string]string {
	return c.sourceModules
}

func (c *Controller) readModulesFromFS(dir string) (map[string]string, error) {
	moduleLinks, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	modules := make(map[string]string, len(moduleLinks))

	for _, moduleLink := range moduleLinks {
		index := strings.Index(moduleLink.Name(), "-")
		if index == -1 {
			continue
		}

		moduleName := moduleLink.Name()[index+1:]
		modules[moduleName] = path.Join(dir, moduleLink.Name())
	}

	return modules, nil
}

// restoreAbsentSourceModules checks ModuleReleases with Deployed status and restore them on the FS
func (c *Controller) restoreAbsentSourceModules() error {
	// directory for symlinks will actual versions to all external-modules
	symlinksDir := filepath.Join(c.externalModulesDir, "modules")

	releaseList, err := c.d8ClientSet.DeckhouseV1alpha1().ModuleReleases().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}

	// TODO: add labels to list only Deployed releases
	for _, item := range releaseList.Items {
		if item.Status.Phase != "Deployed" {
			continue
		}

		moduleDir := filepath.Join(symlinksDir, fmt.Sprintf("%d-%s", item.Spec.Weight, item.Spec.ModuleName))
		_, err = os.Stat(moduleDir)
		if err != nil && os.IsNotExist(err) {
			log.Infof("Module %q is absent on file system. Restoring it from source %q", item.Spec.ModuleName, item.GetModuleSource())
			moduleVersion := "v" + item.Spec.Version.String()
			moduleName := item.Spec.ModuleName
			moduleSource := item.GetModuleSource()

			ms, err := c.moduleSourcesLister.Get(moduleSource)
			if err != nil {
				c.logger.Warnf("ModuleSource %q is absent. Skipping restoration of the module %q", moduleSource, moduleName)
				continue
			}

			md := downloader.NewModuleDownloader(c.externalModulesDir, ms, utils.GenerateRegistryOptions(ms))
			err = md.DownloadByModuleVersion(moduleName, moduleVersion)
			if err != nil {
				log.Warnf("Download module %q with version %s failed: %s. Skipping", moduleName, moduleVersion, err)
				continue
			}

			// restore symlink
			moduleRelativePath := filepath.Join("../", moduleName, moduleVersion)
			symlinkPath := filepath.Join(symlinksDir, fmt.Sprintf("%d-%s", item.Spec.Weight, moduleName))
			err = restoreModuleSymlink(c.externalModulesDir, symlinkPath, moduleRelativePath)
			if err != nil {
				log.Warnf("Create symlink for module %q failed: %s", moduleName, err)
				continue
			}

			log.Infof("Module %s:%s restored", moduleName, moduleVersion)
		}
	}

	return nil
}

func restoreModuleSymlink(externalModulesDir, symlinkPath, moduleRelativePath string) error {
	// make absolute path for versioned module
	moduleAbsPath := filepath.Join(externalModulesDir, strings.TrimPrefix(moduleRelativePath, "../"))
	// check that module exists on a disk
	if _, err := os.Stat(moduleAbsPath); os.IsNotExist(err) {
		return err
	}

	return os.Symlink(moduleRelativePath, symlinkPath)
}

type byVersion []*v1alpha1.ModuleRelease

func (b byVersion) Len() int {
	return len(b)
}

func (b byVersion) Swap(i, j int) {
	b[i], b[j] = b[j], b[i]
}

func (b byVersion) Less(i, j int) bool {
	return b[i].Spec.Version.LessThan(b[j].Spec.Version)
}
