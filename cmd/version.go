/*
 *  Copyright IBM Corporation 2022
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *        http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 */

package cmd

import (
	"fmt"

	api "github.com/konveyor/move2kube-wasm/lib"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// GetVersionCommand returns a command to print the version
func GetVersionCommand() *cobra.Command {
	viper.AutomaticEnv()

	long := false
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version information",
		Long:  "Print the version information",
		Run:   func(*cobra.Command, []string) { fmt.Println(api.GetVersion(long)) },
	}

	versionCmd.Flags().BoolVarP(&long, "long", "l", false, "Print the version details.")

	return versionCmd
}
