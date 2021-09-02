// Copyright 2021 Flant CJSC
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

package app

import (
	"time"

	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	InternalNodeIP = ""
	DevicePath     = ""

	ResourcesPath    = ""
	ResourcesTimeout = "15m"
	DeckhouseTimeout = 10 * time.Minute

	ForceAbortFromCache = false
)

func DefineBashibleBundleFlags(cmd *kingpin.CmdClause) {
	cmd.Flag("internal-node-ip", "Address of a node from internal network.").
		Required().
		Envar(configEnvName("INTERNAL_NODE_IP")).
		StringVar(&InternalNodeIP)
	cmd.Flag("device-path", "Path of kubernetes-data device.").
		Required().
		Envar(configEnvName("DEVICE_PATH")).
		StringVar(&DevicePath)
}

func DefineDeckhouseFlags(cmd *kingpin.CmdClause) {
	cmd.Flag("deckhouse-timeout", "Timeout to install deckhouse. Experimental. This feature may be deleted in the future.").
		Envar(configEnvName("DECKHOUSE_TIMEOUT")).
		Default(DeckhouseTimeout.String()).
		DurationVar(&DeckhouseTimeout)
}

func DefineResourcesFlags(cmd *kingpin.CmdClause, isRequired bool) {
	cmd.Flag("resources", "Path to a file with declared Kubernetes resources in YAML format.").
		Envar(configEnvName("RESOURCES")).
		StringVar(&ResourcesPath)
	cmd.Flag("resources-timeout", "Timeout to create resources. Experimental. This feature may be deleted in the future.").
		Envar(configEnvName("RESOURCES_TIMEOUT")).
		Default(ResourcesTimeout).
		StringVar(&ResourcesTimeout)
	if isRequired {
		cmd.GetFlag("resources").Required()
	}
}

func DefineAbortFlags(cmd *kingpin.CmdClause) {
	const help = `Skip 'use dhctl destroy command' error. It force bootstrap abortion from cache.
Experimental. This feature may be deleted in the future.`
	cmd.Flag("force-abort-from-cache", help).
		Envar(configEnvName("FORCE_ABORT_FROM_CACHE")).
		Default("false").
		BoolVar(&ForceAbortFromCache)
}
