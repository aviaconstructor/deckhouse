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
	"os"
	"path/filepath"

	"gopkg.in/alecthomas/kingpin.v2"
)

const AppName = "dhctl"

var TmpDirName = filepath.Join(os.TempDir(), "dhctl")

var (
	AppVersion = "dev"

	ConfigPath  = ""
	SanityCheck = false
	LoggerType  = "pretty"
	IsDebug     = false
)

func init() {
	if os.Getenv("DHCTL_DEBUG") == "yes" {
		IsDebug = true
	}
}

func GlobalFlags(cmd *kingpin.Application) {
	cmd.Flag("logger-type", "Format logs output of a dhctl in different ways.").
		Envar(configEnvName("LOGGER_TYPE")).
		Default("pretty").
		EnumVar(&LoggerType, "pretty", "simple", "json")
	cmd.Flag("tmp-dir", "Set temporary directory for debug purposes.").
		Envar(configEnvName("TMP_DIR")).
		Default(TmpDirName).
		StringVar(&TmpDirName)
}

func DefineConfigFlags(cmd *kingpin.CmdClause) {
	cmd.Flag("config", "Config file path").
		Required().
		Envar(configEnvName("CONFIG")).
		StringVar(&ConfigPath)
}

func DefineSanityFlags(cmd *kingpin.CmdClause) {
	cmd.Flag("yes-i-am-sane-and-i-understand-what-i-am-doing", "You should double check what you are doing here.").
		Default("false").
		BoolVar(&SanityCheck)
}

func configEnvName(name string) string {
	return "DHCTL_CLI_" + name
}
