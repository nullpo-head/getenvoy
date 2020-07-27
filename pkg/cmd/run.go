// Copyright 2019 Tetrate
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

package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tetratelabs/getenvoy/pkg/binary/envoy"
	"github.com/tetratelabs/getenvoy/pkg/binary/envoy/controlplane"
	"github.com/tetratelabs/getenvoy/pkg/binary/envoy/debug"
	"github.com/tetratelabs/getenvoy/pkg/flavors"
	_ "github.com/tetratelabs/getenvoy/pkg/flavors/postgres" //nolint
	"github.com/tetratelabs/getenvoy/pkg/manifest"
)

var (
	controlplaneAddress    string
	accessLogServerAddress string
	mode                   string
	bootstrap              string
	templateArgs           map[string]string
)

// NewRunCmd create a command responsible for starting an Envoy process
func NewRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run <reference|filepath> [flags] [-- <envoy-args>]",
		Short: "Runs an instance of Envoy.",
		Long: `
Manages full lifecycle of Envoy including bootstrap generation and automated collection of access logs,
Envoy state and machine state into the ` + "`~/.getenvoy/debug`" + ` directory.`,
		Example: `# Run using a manifest reference.
getenvoy run standard:1.11.1 -- --config-path ./bootstrap.yaml

# Run as a gateway using an Istio controlplane bootstrap.
getenvoy run standard:1.11.1 --mode loadbalancer --bootstrap istio --controlplaneAddress istio-pilot.istio-system:15010

# Run using a filepath.
getenvoy run ./envoy -- --config-path ./bootstrap.yaml

# List available Envoy flags.
getenvoy run standard:1.11.1 -- --help

# Run with Postgres specific configuration bootstrapped
getenvoy run postgres:nightly --templateArg endpoint=127.0.0.1:5432 --templateArg inport=5555
`,
		Args: func(cmd *cobra.Command, args []string) error {
			return validateCmdArgs(args)

		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := envoy.NewConfig(
				func(c *envoy.Config) {
					c.XDSAddress = controlplaneAddress
					c.Mode = envoy.ParseMode(mode)
					c.ALSAddresss = accessLogServerAddress
				},
			)

			runtime, err := envoy.NewRuntime(
				func(r *envoy.Runtime) { r.Config = cfg },
				debug.EnableEnvoyAdminDataCollection,
				debug.EnableEnvoyLogCollection,
				debug.EnableNodeCollection,
				debug.EnableOpenFilesDataCollection,
				controlplaneFunc(),
			)
			if err != nil {
				return err
			}

			key, manifestErr := manifest.NewKey(args[0])

			if manifestErr != nil {
				if _, err := os.Stat(args[0]); err != nil {
					return fmt.Errorf("%v isn't valid manifest reference or an existing filepath", args[0])
				}
				return runtime.RunPath(args[0], args[1:])
			}

			// Check if the templateArgs were passed to the cmd line.
			// If they were passed, config must be created based on
			// template.
			if len(templateArgs) > 0 {
				cmdArg, err := processTemplateArgs(key.Flavor, args, templateArgs, runtime.(*envoy.Runtime))
				if err != nil {
					return err
				}
				args = append(args, cmdArg)
			}

			if !runtime.AlreadyDownloaded(key) {
				location, err := manifest.Locate(key, manifestURL)
				if err != nil {
					return err
				}
				if err := runtime.Fetch(key, location); err != nil {
					return err
				}
			}
			return runtime.Run(key, args[1:])
		},
	}
	cmd.Flags().StringVarP(&bootstrap, "bootstrap", "b", "",
		fmt.Sprintf("(experimental) controlplane bootstrap to generate and use <%v>", strings.Join(supported, "|")))
	cmd.Flags().StringVar(&controlplaneAddress, "controlplaneAddress", "",
		"(experimental) location of Envoy's dynamic configuration server <host|ip:port> (requires bootstrap flag)")
	cmd.Flags().StringVar(&accessLogServerAddress, "accessLogServerAddress", "",
		"(experimental) location of Envoy's access log server <host|ip:port> (requires bootstrap flag)")
	cmd.Flags().StringVar(&mode, "mode", "",
		fmt.Sprintf("(experimental) mode to run Envoy in <%v> (requires bootstrap flag)", strings.Join(envoy.SupportedModes, "|")))
	cmd.Flags().StringToStringVar(&templateArgs, "templateArg", map[string]string{},
		"arguments passed to a config template for substitution")
	return cmd
}

var (
	istio = "istio"

	supported         = []string{istio}
	requiresBootstrap = []*string{&controlplaneAddress, &accessLogServerAddress, &mode}
)

func validateBootstrap() error {
	for _, bs := range append(supported, "") {
		if bs == bootstrap {
			return nil
		}
	}
	return fmt.Errorf("unsupported bootstrap %v, must be one of (%v)", bootstrap, strings.Join(supported, "|"))
}

func validateMode() error {
	for _, m := range append(envoy.SupportedModes, "") {
		if m == mode {
			return nil
		}
	}
	return fmt.Errorf("unsupported mode %v, must be one of (%v)", mode, strings.Join(envoy.SupportedModes, "|"))
}

func validateRequiresBootstrap() error {
	if bootstrap == "" {
		for i := range requiresBootstrap {
			if *requiresBootstrap[i] != "" {
				return fmt.Errorf("--%v requires --bootstrap to be set", *requiresBootstrap[i])
			}
		}
	}
	return nil
}

func validateCmdArgs(args []string) error {
	if len(args) == 0 {
		return errors.New("missing binary parameter")
	}
	if err := validateMode(); err != nil {
		return err
	}
	if err := validateBootstrap(); err != nil {
		return err
	}
	return validateRequiresBootstrap()
}

func controlplaneFunc() func(r *envoy.Runtime) {
	switch bootstrap {
	case istio:
		return controlplane.Istio
	default:
		// do nothing...
		return func(r *envoy.Runtime) {}
	}
}

// Function verifies template parameters passed in getenvoy command line.
// If verification is successful, it returns a string which must be added to
// Envoy command line to invoke a proper config.
func processTemplateArgs(flavor string, args []string, templateArgs map[string]string, runtime *envoy.Runtime) (string, error) {
	for _, envoyParam := range args {
		if strings.HasPrefix(envoyParam, "--config") {
			return "", fmt.Errorf("--templateArg and %s cannot be specified at the same time", envoyParam)
		}
	}
	config, err := flavors.CreateConfig(flavor, templateArgs)
	if err != nil {
		return "", err
	}
	// Save config in getenvoy directory
	path, err := runtime.SaveConfig(flavor, config)
	if err != nil {
		return "", err
	}
	return "--config-path " + path, nil
}
