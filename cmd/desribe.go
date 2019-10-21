// Copyright © 2019 IBM Corporation and others.
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
	"fmt"
	"strings"
	//"encoding/json"
	//"gopkg.in/yaml.v2"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

type stackInfo struct {
	name string
	version string
}

type probeInfo struct {
	name string
	failureThreshold int
	scheme string
	path string
	initialDelay int
	period int
	succcessThreshold int
	timeoutSeconds int
}

type deploymentInfo struct {
	clusterName string
	nodeName string
	namespace string
	podName string
	phase string
	hostIP string
	startTime string
}

type describeInfo struct {
	stack stackInfo
	deployment deploymentInfo
	liveness probeInfo
	readiness probeInfo
	container containerInfo
}

type containerInfo struct {
	containerName string
	imageNameAndTag string
	exposedPorts []string
	envVars map[string]string
	restartCount int
	volumeMounts map[string]string
}

type describeCommandConfig struct {
	*RootCommandConfig
	outputFormat            string
}

func newDescribeCmd(rootConfig *RootCommandConfig) *cobra.Command {
	config := &describeCommandConfig{RootCommandConfig: rootConfig}

	var describeCmd = &cobra.Command{
		Use:   "describe",
		Short: "Describe an appsody application",
		Long: `This command describes the static and dynamic attributes, pertaining to the appsody stack, the image, and the container(if there is one).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return describe(config)
		},
	}

	describeCmd.PersistentFlags().StringVarP(&config.outputFormat, "output-format", "o", "yaml", "The desired output format - json or yaml")
	return describeCmd
}

func getRunningPod(appName string) (string, error) {
        selector := fmt.Sprintf("app.kubernetes.io/name=%s", appName)
	cmdParms := []string{"pods", "-l", selector, "-o", "name"}
        return KubeGet(cmdParms, "default", false)
}

func describe(config *describeCommandConfig) error {
	outputFormat := strings.ToLower(config.outputFormat) 
	if outputFormat != "yaml" && outputFormat != "json" {
		return errors.Errorf("Invalid format string - %s. Please use \"json\" or \"yaml\" only", outputFormat)
	}
	podName, _ := getRunningPod("test8")
	cmdParms := []string{strings.TrimSuffix(podName, "\n"), "-o", outputFormat}
	description, _ := KubeGet(cmdParms, "default", false)
        fmt.Println(description)
	return nil 
}
