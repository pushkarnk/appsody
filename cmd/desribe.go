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
	"gopkg.in/yaml.v2"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

type EnvVar struct {
	name string `yaml:"name"`
	value string `yaml:"value"`
}

type VolumeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
}

type AppInfo struct {
	StackInfo struct {	
		Name string `yaml:"name"`
		Version string `yaml:"version"`
	} `yaml:"stackInfo"`
	DeploymentInfo struct {
		NodeName string	   `yaml:"nodeName" json:"nodeName"`				
		PodName string	   `yaml:"podName" json:"podName"`
		Phase string	   `yaml:"phase" json:"phase"`
		HostIP string	   `yaml:"hostIP" json:"hostIP"`
		StartTime string   `yaml:"startTime" json:"startTime"`
        	LivenessProbe string `yaml:"livenessProbe,flow" json:"livenessProbe"`
        	ReadinessProbe string `yaml:"readinessProbe" json:"readinessProbe"`
	} `yaml:"deploymentInfo" json:"deploymentInfo"`
	ContainerInfo struct {
	        ContainerName string 	       `yaml:"name" json:"name"`
        	ImageName string               `yaml:"imageName" json:"imageName"`
        	ExposedPorts []string	       `yaml:"exposedPorts" json:"exposedPorts"`
        	EnvVars []EnvVar      `yaml:"envVars" json:"envVars"`
        	RestartCount int 	       `yaml:"restartCount" json:"restartCount"`
        	VolumeMounts []VolumeMount `yaml:"volumenMounts" json:"volumeMounts"`
	} `yaml:"containerInfo" json:"containerInfo"`
}

var template = `
stackInfo:
  name: unknown 
  version: unknown 
deploymentInfo:
  nodeName: unknown 
  phase: unknown 
  hostIP: unknown 
  startTime: unknown 
  livenessProbe: unknown
  readinessProbe: unknown 
containerInfo;
  containerName: unknown
  imageName: unknown
  restartCount: 0
`

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

	info := AppInfo{}
	err := yaml.Unmarshal([]byte(template), &info)
	if err == nil {
		fmt.Printf("%v\n", info)
	}

	output, _ := KubeGet([]string{strings.TrimSuffix(podName, "\n"), "-o", "yaml"}, "default", false)
	var data map[string]interface{}
	error := yaml.Unmarshal([]byte(output), &data)	
	if error != nil {
		fmt.Println(error)
	}

        populateStackInfo(&info, data)
	populateDeploymentInfo(&info, data)
	populateProbesInfo(&info, strings.TrimSuffix(podName, "\n"), "default")

        yaml, err := yaml.Marshal(&info)
        if err == nil {
		fmt.Println(string(yaml))
	}
	return nil 
}

func populateProbesInfo(info *AppInfo, podname string, namespace string) {
	livenessProbe, readinessProbe := getProbes(podname, namespace)
        info.DeploymentInfo.LivenessProbe = strings.ReplaceAll(strings.ReplaceAll(livenessProbe, " ","|"), "#", "")
	info.DeploymentInfo.ReadinessProbe = strings.ReplaceAll(strings.ReplaceAll(readinessProbe, " ", "|"), "#", "")
}

func populateStackInfo(info *AppInfo, data map[string]interface{}) {
	if metadata, ok := data["metadata"].(map[interface{}]interface{}); ok {
		if labels, ok := metadata["labels"].(map[interface{}]interface{}); ok {
			if name, ok := labels["app.appsody.dev/stack"].(string); ok {			
				info.StackInfo.Name = name
			}

			if version, ok := labels["app.kubernetes.io/version"].(string); ok {
				info.StackInfo.Version = version 
			}
		}
	}
}


func populateDeploymentInfo(info *AppInfo, data map[string]interface{}) {
	if spec, ok := data["spec"].(map[interface{}]interface{}); ok {
		if nodeName, ok := spec["nodeName"].(string); ok {
			info.DeploymentInfo.NodeName = nodeName
		}
	}

	if status, ok := data["status"].(map[interface{}]interface{}); ok {
		if phase, ok := status["phase"].(string); ok {
			info.DeploymentInfo.Phase = phase
		}

		if hostIP, ok := status["hostIP"].(string); ok {
			info.DeploymentInfo.HostIP = hostIP
		}

		if startTime, ok := status["startTime"].(string); ok {
			info.DeploymentInfo.StartTime = startTime
		}
	}

	if metadata, ok := data["metadata"].(map[interface{}]interface{}); ok {
		if name, ok := metadata["name"].(string); ok {
			info.DeploymentInfo.PodName = name
		}
	}
}

func getProbes(podname string, namespace string) (liveness string, readiness string) {
	var livenessProbe = "unknown"
	var readinessProbe = "unknown"
	output, kerror := KubeDescribe(podname, namespace)
	if kerror != nil {
		fmt.Println(kerror)
		return livenessProbe, readinessProbe
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "Readiness:") {
			readinessProbe = strings.TrimSpace(strings.SplitAfter(line, "Readiness:")[1])
		}
		if strings.Contains(line, "Liveness:") {
			livenessProbe = strings.TrimSpace(strings.SplitAfter(line, "Liveness:")[1])
		}
	}
	return livenessProbe, readinessProbe
}
