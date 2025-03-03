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
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v2"
)

type ProjectConfig struct {
	Platform string
}

type NotAnAppsodyProject string

func (e NotAnAppsodyProject) Error() string { return string(e) }

const ConfigFile = ".appsody-config.yaml"

const LatestVersionURL = "https://github.com/appsody/appsody/releases/latest"

const workDirNotSet = ""

// Checks whether an inode (it does not bother
// about file or folder) exists or not.
func Exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}

func GetEnvVar(searchEnvVar string, config *RootCommandConfig) (string, error) {
	if config.cachedEnvVars == nil {
		config.cachedEnvVars = make(map[string]string)
	}

	if value, present := config.cachedEnvVars[searchEnvVar]; present {
		Debug.logf("Environment variable found cached: %s Value: %s", searchEnvVar, value)
		return value, nil
	}

	// Docker and Buildah produce slightly different output
	// for `inspect` command. Array of maps vs. maps
	var dataBuildah map[string]interface{}
	var dataDocker []map[string]interface{}
	projectConfig, projectConfigErr := getProjectConfig(config)
	if projectConfigErr != nil {
		return "", projectConfigErr
	}
	imageName := projectConfig.Platform
	pullErrs := pullImage(imageName, config)
	if pullErrs != nil {
		return "", pullErrs
	}
	cmdName := "docker"
	cmdArgs := []string{"image", "inspect", imageName}
	if config.Buildah {
		cmdName = "buildah"
		cmdArgs = []string{"inspect", "--format={{.Config}}", imageName}
	}

	inspectCmd := exec.Command(cmdName, cmdArgs...)
	inspectOut, inspectErr := inspectCmd.Output()
	if inspectErr != nil {
		return "", errors.Errorf("Could not inspect the image: %v", inspectErr)

	}

	var err error
	var envVars []interface{}
	if config.Buildah {
		err = json.Unmarshal([]byte(inspectOut), &dataBuildah)
		if err != nil {
			return "", errors.New("error unmarshaling data from inspect command - exiting")
		}
		buildahConfig := dataBuildah["config"].(map[string]interface{})
		envVars = buildahConfig["Env"].([]interface{})

	} else {
		err = json.Unmarshal([]byte(inspectOut), &dataDocker)
		if err != nil {
			return "", errors.New("error unmarshaling data from inspect command - exiting")

		}
		dockerConfig := dataDocker[0]["Config"].(map[string]interface{})

		envVars = dockerConfig["Env"].([]interface{})
	}

	Debug.log("Number of environment variables in stack image: ", len(envVars))
	Debug.log("All environment variables in stack image: ", envVars)
	var varFound = false
	for _, envVar := range envVars {
		nameValuePair := strings.SplitN(envVar.(string), "=", 2)
		name, value := nameValuePair[0], nameValuePair[1]
		config.cachedEnvVars[name] = value
		if name == searchEnvVar {
			varFound = true
		}
	}
	if varFound {
		Debug.logf("Environment variable found: %s Value: %s", searchEnvVar, config.cachedEnvVars[searchEnvVar])
		return config.cachedEnvVars[searchEnvVar], nil
	}
	Debug.log("Could not find env var: ", searchEnvVar)
	return "", nil
}

func getEnvVarBool(searchEnvVar string, config *RootCommandConfig) (bool, error) {
	strVal, envErr := GetEnvVar(searchEnvVar, config)
	if envErr != nil {
		return false, envErr
	}
	return strings.Compare(strings.TrimSpace(strings.ToUpper(strVal)), "TRUE") == 0, nil
}

func getEnvVarInt(searchEnvVar string, config *RootCommandConfig) (int, error) {

	strVal, envErr := GetEnvVar(searchEnvVar, config)
	if envErr != nil {
		return 0, envErr
	}
	intVal, err := strconv.Atoi(strVal)
	if err != nil {
		return 0, err
	}
	return intVal, nil

}

func getExtractDir(config *RootCommandConfig) (string, error) {
	extractDir, envErr := GetEnvVar("APPSODY_PROJECT_DIR", config)
	if envErr != nil {
		return "", envErr
	}
	if extractDir == "" {
		Warning.log("The stack image does not contain APPSODY_PROJECT_DIR. Using /project")
		return "/project", nil
	}
	return extractDir, nil
}

func getVolumeArgs(config *RootCommandConfig) ([]string, error) {
	volumeArgs := []string{}
	stackMounts, envErr := GetEnvVar("APPSODY_MOUNTS", config)
	if envErr != nil {
		return nil, envErr
	}
	if stackMounts == "" {
		Warning.log("The stack image does not contain APPSODY_MOUNTS")
		return volumeArgs, nil
	}
	stackMountList := strings.Split(stackMounts, ";")
	homeDir := UserHomeDir()
	homeDirOverride := os.Getenv("APPSODY_MOUNT_HOME")
	homeDirOverridden := false
	if homeDirOverride != "" {
		Debug.logf("Overriding home mount dir from '%s' to APPSODY_MOUNT_HOME value '%s' ", homeDir, homeDirOverride)
		homeDir = homeDirOverride
		homeDirOverridden = true
	}
	projectDir, perr := getProjectDir(config)
	if perr != nil {
		return volumeArgs, perr

	}
	projectDirOverride := os.Getenv("APPSODY_MOUNT_PROJECT")
	projectDirOverridden := false
	if projectDirOverride != "" {
		Debug.logf("Overriding project mount dir from '%s' to APPSODY_MOUNT_PROJECT value '%s' ", projectDir, projectDirOverride)
		projectDir = projectDirOverride
		projectDirOverridden = true
	}

	for _, mount := range stackMountList {
		if mount == "" {
			continue
		}
		var mappedMount string
		var overridden bool
		if strings.HasPrefix(mount, "~") {
			mappedMount = strings.Replace(mount, "~", homeDir, 1)
			overridden = homeDirOverridden
		} else {
			mappedMount = filepath.Join(projectDir, mount)
			overridden = projectDirOverridden
		}
		// mappedMount contains local and container (linux) paths. When on windows, the Join above replaces all '/' with '\' and
		// breaks the linux paths. This method is to always use '/' because windows docker tolerates this.
		mappedMount = filepath.ToSlash(mappedMount)

		if !overridden && !mountExistsLocally(mappedMount) {
			Warning.log("Could not mount ", mappedMount, " because the local file was not found.")
			continue
		}
		volumeArgs = append(volumeArgs, "-v", mappedMount)
	}
	Debug.log("Mapped mount args: ", volumeArgs)
	return volumeArgs, nil
}

func mountExistsLocally(mount string) bool {
	localFile := strings.Split(mount, ":")
	if runtime.GOOS == "windows" {
		//Windows may prepend the drive ID to the path
		//ex. C:\whatever\path\:/linux/dir
		if len(localFile) > 2 {
			//This is the case where we have three strings, the first one being
			//the drive ID
			// C: \whatever\path and /linux/dir
			localFile[0] += ":" + localFile[1]
			//We append the second string to the first
			//thus reconstituting the entire local path
		}
	}
	Debug.log("Checking for existence of local file or directory to mount: ", localFile[0])
	fileExists, _ := Exists(localFile[0])
	return fileExists
}

func getProjectDir(config *RootCommandConfig) (string, error) {
	appsodyConfig := filepath.Join(config.ProjectDir, ConfigFile)
	projectDir, err := Exists(appsodyConfig)
	if err != nil {
		Error.log(err)
		return "", err
	}
	if !projectDir {
		var e NotAnAppsodyProject = "The current directory is not a valid appsody project. Run `appsody init <stack>` to create one. Run `appsody list` to see the available stacks."
		return "", &e
	}
	return config.ProjectDir, nil
}

func getProjectConfig(config *RootCommandConfig) (ProjectConfig, error) {
	if config.ProjectConfig == nil {
		dir, perr := getProjectDir(config)
		if perr != nil {
			var tempProjectConfig ProjectConfig
			return tempProjectConfig, errors.Errorf("The current directory is not a valid appsody project. Run appsody init <stack> to create one: %v", perr)

		}
		appsodyConfig := filepath.Join(dir, ConfigFile)
		v := viper.New()
		v.SetConfigFile(appsodyConfig)
		Debug.log("Project config file set to: ", appsodyConfig)
		err := v.ReadInConfig()
		if err != nil {
			var tempProjectConfig ProjectConfig
			return tempProjectConfig, errors.Errorf("Error reading project config %v", err)

		}
		stack := v.GetString("stack")
		Debug.log("Project stack from config file: ", stack)
		imageRepo := config.CliConfig.GetString("images")
		Debug.log("Image repository set to: ", imageRepo)
		if imageRepo != "index.docker.io" {
			stack = imageRepo + "/" + stack
		}
		Debug.log("Pulling stack image as: ", stack)
		config.ProjectConfig = &ProjectConfig{stack}
	}
	return *config.ProjectConfig, nil
}

func getOperatorHome(config *RootCommandConfig) string {
	operatorHome := config.CliConfig.GetString("operator")
	Debug.log("Operator home set to: ", operatorHome)
	return operatorHome
}

func getProjectName(config *RootCommandConfig) (string, error) {
	projectDir, err := getProjectDir(config)
	if err != nil {
		return "my-project", err
	}
	projectName := strings.ToLower(filepath.Base(projectDir))
	projectName = strings.ReplaceAll(projectName, "_", "-")
	return projectName, nil
}

func execAndWait(command string, args []string, logger appsodylogger, dryrun bool) error {

	return execAndWaitWithWorkDir(command, args, logger, workDirNotSet, dryrun)
}
func execAndWaitWithWorkDir(command string, args []string, logger appsodylogger, workdir string, dryrun bool) error {

	err := execAndWaitWithWorkDirReturnErr(command, args, logger, workdir, dryrun)
	if err != nil {
		return errors.Errorf("Error running %s command: %v", command, err)

	}
	return nil

}

// CopyFile uses OS commands to copy a file from a source to a destination
func CopyFile(source string, dest string) error {
	_, err := os.Stat(source)
	if err != nil {
		Error.logf("Cannot find source file %s to copy", source)
		return err
	}

	var execCmd string
	var execArgs = []string{source, dest}

	if runtime.GOOS == "windows" {
		execCmd = "CMD"
		winArgs := []string{"/C", "COPY"}
		execArgs = append(winArgs[0:], execArgs...)

	} else {
		execCmd = "cp"
	}
	copyCmd := exec.Command(execCmd, execArgs...)
	cmdOutput, cmdErr := copyCmd.Output()
	_, err = os.Stat(dest)
	if err != nil {
		Error.logf("Could not copy %s to %s - output of copy command %s %s\n", source, dest, cmdOutput, cmdErr)
		return errors.New("Error in copy: " + cmdErr.Error())
	}
	Debug.logf("Copy of %s to %s was successful \n", source, dest)
	return nil
}

// MoveDir moves a directory to another directory, even if they are on different partitions
func MoveDir(fromDir string, toDir string) error {
	Debug.log("Moving ", fromDir, " to ", toDir)
	// Let's try os.Rename first
	err := os.Rename(fromDir, toDir)
	if err == nil {
		// We did it - returning
		//Error.log("Could not move ", extractDir, " to ", targetDir, " ", err)
		return nil
	}
	// If we are here, we need to use copy
	Debug.log("os.Rename did not work to move directories... attempting copy. From dir:", fromDir, " target dir: ", toDir)
	err = copyDir(fromDir, toDir)
	if err != nil {
		Error.log("Could not move ", fromDir, " to ", toDir)
		return err
	}
	return nil
}

func copyDir(fromDir string, toDir string) error {
	_, err := os.Stat(fromDir)
	if err != nil {
		Error.logf("Cannot find source directory %s to copy", fromDir)
		return err
	}

	var execCmd string
	var execArgs = []string{fromDir, toDir}

	if runtime.GOOS == "windows" {
		execCmd = "CMD"
		winArgs := []string{"/C", "XCOPY", "/I", "/E", "/H", "/K"}
		execArgs = append(winArgs[0:], execArgs...)

	} else {
		execCmd = "cp"
		bashArgs := []string{"-rf"}
		execArgs = append(bashArgs[0:], execArgs...)
	}
	Debug.log("About to run: ", execCmd, execArgs)
	copyCmd := exec.Command(execCmd, execArgs...)
	cmdOutput, cmdErr := copyCmd.Output()
	_, err = os.Stat(toDir)
	if err != nil {
		Error.logf("Could not copy %s to %s - output of copy command %s %s\n", fromDir, toDir, cmdOutput, cmdErr)
		return errors.New("Error in copy: " + cmdErr.Error())
	}
	Debug.logf("Directory copy of %s to %s was successful \n", fromDir, toDir)
	return nil
}

// CheckPrereqs checks the prerequisites to run the CLI
func CheckPrereqs() error {
	dockerCmd := "docker"
	dockerArgs := []string{"ps"}
	checkDockerCmd := exec.Command(dockerCmd, dockerArgs...)
	_, cmdErr := checkDockerCmd.Output()
	if cmdErr != nil {
		return errors.New("docker does not seem to be installed or running - failed to execute docker ps")
	}
	return nil
}

// UserHomeDir returns the current user's home directory or '.'
func UserHomeDir() string {
	homeDir, homeErr := os.UserHomeDir()

	if homeErr != nil {
		Error.log("Unable to find user's home directory", homeErr)
		return "."
	}
	return homeDir
}

func getExposedPorts(config *RootCommandConfig) ([]string, error) {
	// TODO cache this so the docker inspect command only runs once per cli invocation
	var data []map[string]interface{}
	var buildahData map[string]interface{}
	var portValues []string
	projectConfig, projectConfigErr := getProjectConfig(config)
	if projectConfigErr != nil {
		return nil, projectConfigErr
	}
	imageName := projectConfig.Platform
	pullErrs := pullImage(imageName, config)
	if pullErrs != nil {
		return nil, pullErrs
	}

	cmdName := "docker"
	cmdArgs := []string{"image", "inspect", imageName}
	if config.Buildah {
		cmdName = "buildah"
		cmdArgs = []string{"inspect", "--format", "{{.Config}}", imageName}
	}
	Debug.Logf("About to run %s with args %s ", cmdName, cmdArgs)
	inspectCmd := exec.Command(cmdName, cmdArgs...)
	inspectOut, inspectErr := inspectCmd.Output()
	if inspectErr != nil {
		return portValues, errors.Errorf("Could not inspect the image: %v", inspectErr)
	}
	if config.Buildah {
		err := json.Unmarshal([]byte(inspectOut), &buildahData)
		if err != nil {
			return portValues, errors.Errorf("Error unmarshaling data from inspect command - exiting %v", err)
		}
	} else {
		err := json.Unmarshal([]byte(inspectOut), &data)
		if err != nil {
			return portValues, errors.Errorf("Error unmarshaling data from inspect command - exiting %v", err)
		}
	}
	var containerConfig map[string]interface{}

	if config.Buildah {
		containerConfig = buildahData["config"].(map[string]interface{})
		Debug.Log("Config inspected by buildah: ", config)
	} else {
		containerConfig = data[0]["Config"].(map[string]interface{})
	}

	if containerConfig["ExposedPorts"] != nil {
		exposedPorts := containerConfig["ExposedPorts"].(map[string]interface{})

		portValues = make([]string, 0, len(exposedPorts))
		for k := range exposedPorts {
			portValues = append(portValues, strings.Split(k, "/tcp")[0])
		}

	}
	return portValues, nil

}

//GenKnativeYaml generates a simple yaml for KNative serving
func GenKnativeYaml(yamlTemplate string, deployPort int, serviceName string, deployImage string, pullImage bool, configFile string, dryrun bool) (fileName string, yamlErr error) {
	// KNative serving YAML representation in a struct
	type Y struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace,omitempty"`
		} `yaml:"metadata"`
		Spec struct {
			RunLatest struct {
				Configuration struct {
					RevisionTemplate struct {
						Spec struct {
							Container struct {
								Image           string           `yaml:"image"`
								ImagePullPolicy string           `yaml:"imagePullPolicy"`
								Ports           []map[string]int `yaml:"ports"`
							} `yaml:"container"`
						} `yaml:"spec"`
					} `yaml:"revisionTemplate"`
				} `yaml:"configuration"`
			} `yaml:"runLatest"`
		} `yaml:"spec"`
	}
	yamlMap := Y{}
	err := yaml.Unmarshal([]byte(yamlTemplate), &yamlMap)
	//Set the name
	yamlMap.Metadata.Name = serviceName
	//Set the image
	yamlMap.Spec.RunLatest.Configuration.RevisionTemplate.Spec.Container.Image = deployImage
	//Set the image pull policy to Never if we're not pushing an image to a registry
	if !pullImage {
		yamlMap.Spec.RunLatest.Configuration.RevisionTemplate.Spec.Container.ImagePullPolicy = "Never"
	}
	//Set the containerPort
	ports := yamlMap.Spec.RunLatest.Configuration.RevisionTemplate.Spec.Container.Ports
	if len(ports) > 1 {
		//KNative only allows a single port entry
		Warning.log("KNative yaml template defines more than one port. This is invalid.")
	}

	if len(ports) >= 1 {
		found := false
		for _, thePort := range ports {
			Debug.log("Detected KNative template port: ", thePort)
			_, found = thePort["containerPort"]
			if found {
				Debug.log("YAML template defined a single port - setting it to: ", deployPort)
				thePort["containerPort"] = deployPort
				break
			}
		}
		if !found {
			//This template is invalid because the only value that's allowed is containerPort
			Warning.log("The Knative template defines a port with a key other than containerPort. This is invalid.")
			Warning.log("Adding containerPort - you will have to edit the yaml file manually.")
			newPort := map[string]int{"containerPort": deployPort}
			ports = append(ports, newPort)
			yamlMap.Spec.RunLatest.Configuration.RevisionTemplate.Spec.Container.Ports = ports
		}
	} else { //no ports defined
		var newPorts [1]map[string]int
		newPorts[0] = map[string]int{"containerPort": deployPort}
		yamlMap.Spec.RunLatest.Configuration.RevisionTemplate.Spec.Container.Ports = newPorts[:]
	}
	if err != nil {
		Error.log("Could not create the YAML structure from template. Exiting.")
		return "", err
	}
	Debug.logf("YAML map: \n%v\n", yamlMap)
	yamlStr, err := yaml.Marshal(&yamlMap)
	if err != nil {
		Error.log("Could not create the YAML string from Map. Exiting.")
		return "", err
	}
	Debug.logf("Generated YAML: \n%s\n", yamlStr)
	// Generate file based on supplied config, defaulting to app-deploy.yaml
	yamlFile := configFile
	if dryrun {
		Info.log("Skipping creation of yaml file with prefix: ", yamlFile)
		return yamlFile, nil
	}
	err = ioutil.WriteFile(yamlFile, yamlStr, 0666)
	if err != nil {
		return "", fmt.Errorf("Could not create the yaml file for KNative deployment %v", err)
	}
	return yamlFile, nil
}

//GenDeploymentYaml generates a simple yaml for a plaing K8S deployment
func GenDeploymentYaml(appName string, imageName string, ports []string, pdir string, projectLabel string, dryrun bool) (fileName string, err error) {
	// KNative serving YAML representation in a struct
	type Port struct {
		Name          string `yaml:"name,omitempty"`
		ContainerPort int    `yaml:"containerPort"`
	}
	type EnvVar struct {
		Name  string `yaml:"name,omitempty"`
		Value string `yaml:"value,omitempty"`
	}
	type VolumeMount struct {
		Name      string `yaml:"name"`
		MountPath string `yaml:"mountPath"`
		SubPath   string `yaml:"subPath,omitempty"`
	}
	type Container struct {
		Args            []string  `yaml:"args,omitempty"`
		Command         []string  `yaml:"command,omitempty"`
		Env             []*EnvVar `yaml:"env,omitempty"`
		Image           string    `yaml:"image"`
		ImagePullPolicy string    `yaml:"imagePullPolicy,omitempty"`
		Name            string    `yaml:"name,omitempty"`
		Ports           []*Port   `yaml:"ports,omitempty"`
		SecurityContext struct {
			Privileged bool `yaml:"privileged"`
		} `yaml:"securityContext,omitempty"`
		VolumeMounts []VolumeMount `yaml:"volumeMounts"`
		WorkingDir   string        `yaml:"workingDir,omitempty"`
	}
	type Volume struct {
		Name                  string `yaml:"name"`
		PersistentVolumeClaim struct {
			ClaimName string `yaml:"claimName"`
		} `yaml:"persistentVolumeClaim,omitempty"`
		EmptyDir struct {
			Medium string `yaml:"medium"`
		} `yaml:"emptyDir,omitempty"`
	}

	type Deployment struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace,omitempty"`
		} `yaml:"metadata"`
		Spec struct {
			Selector struct {
				MatchLabels map[string]string `yaml:"matchLabels"`
			} `yaml:"selector"`
			Replicas    int `yaml:"replicas"`
			PodTemplate struct {
				Metadata struct {
					Labels map[string]string `yaml:"labels"`
				} `yaml:"metadata"`
				Spec struct {
					ServiceAccountName string       `yaml:"serviceAccountName,omitempty"`
					Containers         []*Container `yaml:"containers"`
					Volumes            []*Volume    `yaml:"volumes"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}

	yamlMap := Deployment{}
	yamlTemplate := getDeploymentTemplate()
	err = yaml.Unmarshal([]byte(yamlTemplate), &yamlMap)
	if err != nil {
		Error.log("Could not create the YAML structure from template. Exiting.")
		return "", err
	}
	//Set the name
	yamlMap.Metadata.Name = appName
	//Set the image
	yamlMap.Spec.PodTemplate.Spec.Containers[0].Name = appName
	yamlMap.Spec.PodTemplate.Spec.Containers[0].Image = imageName

	//Set the containerPort
	containerPorts := make([]*Port, 0)
	for i, port := range ports {
		//KNative only allows a single port entry
		if i == 0 {
			yamlMap.Spec.PodTemplate.Spec.Containers[0].Ports = containerPorts
		}
		Debug.Log("Adding port to yaml: ", port)
		newContainerPort := new(Port)
		newContainerPort.ContainerPort, err = strconv.Atoi(port)
		if err != nil {
			return "", err
		}
		yamlMap.Spec.PodTemplate.Spec.Containers[0].Ports = append(yamlMap.Spec.PodTemplate.Spec.Containers[0].Ports, newContainerPort)
	}
	//Set the workspace volume mount

	subPath := filepath.Base(pdir)
	workspaceMount := VolumeMount{"appsody-workspace", "/project/user-app", subPath}
	yamlMap.Spec.PodTemplate.Spec.Containers[0].VolumeMounts = append(yamlMap.Spec.PodTemplate.Spec.Containers[0].VolumeMounts, workspaceMount)
	//Set the deployment selector and pod label

	if err != nil {
		return "", err
	}
	yamlMap.Spec.Selector.MatchLabels["app"] = projectLabel
	yamlMap.Spec.PodTemplate.Metadata.Labels["app"] = projectLabel

	Debug.logf("YAML map: \n%v\n", yamlMap)
	yamlStr, err := yaml.Marshal(&yamlMap)
	if err != nil {
		Error.log("Could not create the YAML string from Map. Exiting.")
		return "", err
	}
	Debug.logf("Generated YAML: \n%s\n", yamlStr)
	// Generate file based on supplied config, defaulting to app-deploy.yaml
	yamlFile := filepath.Join(pdir, "app-deploy.yaml")
	if dryrun {
		Info.log("Skipping creation of yaml file with prefix: ", yamlFile)
		return yamlFile, nil
	}
	err = ioutil.WriteFile(yamlFile, yamlStr, 0666)
	if err != nil {
		return "", fmt.Errorf("Could not create the yaml file for deployment %v", err)
	}
	return yamlFile, nil
}
func getDeploymentTemplate() string {
	yamltempl := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: APPSODY_APP_NAME
spec:
  selector:
    matchLabels:
      app: appsody
  replicas: 1
  template:
    metadata:
      labels:
        app: appsody
    spec:
      serviceAccountName: appsody-sa
      containers:
      - name: APPSODY_APP_NAME
        image: APPSODY_STACK
        imagePullPolicy: Always
        command: ["/.appsody/appsody-controller"]
        env:
          - name: HOME
            value: /.appsody
        volumeMounts:
          - name: appsody-controller
            mountPath: /.appsody
      volumes:
        - name: appsody-controller
          persistentVolumeClaim: 
            claimName: appsody-controller
        - name: appsody-workspace
          persistentVolumeClaim: 
            claimName: appsody-workspace
`
	return yamltempl
}

//GenServiceYaml returns the file name of a generated K8S Service yaml
func GenServiceYaml(appName string, ports []string, pdir string, dryrun bool) (fileName string, err error) {
	type Port struct {
		Name       string `yaml:"name,omitempty"`
		Port       int    `yaml:"port"`
		TargetPort int    `yaml:"targetPort"`
	}
	type Service struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec struct {
			Selector    map[string]string `yaml:"selector"`
			ServiceType string            `yaml:"type"`
			Ports       []Port            `yaml:"ports"`
		} `yaml:"spec"`
	}

	var service Service
	service.APIVersion = "v1"
	service.Kind = "Service"
	service.Metadata.Name = fmt.Sprintf("%s-%s", appName, "service")
	service.Spec.Selector = make(map[string]string, 1)
	service.Spec.Selector["app"] = appName
	service.Spec.ServiceType = "NodePort"
	service.Spec.Ports = make([]Port, len(ports))
	for i, port := range ports {
		service.Spec.Ports[i].Name = fmt.Sprintf("port-%d", i)
		iPort, err := strconv.Atoi(port)
		if err != nil {
			return "", err
		}
		service.Spec.Ports[i].Port = iPort
		service.Spec.Ports[i].TargetPort = iPort
	}

	yamlStr, err := yaml.Marshal(&service)
	if err != nil {
		Error.log("Could not create the YAML string from Map. Exiting.")
		return "", err
	}
	Debug.logf("Generated YAML: \n%s\n", yamlStr)
	// Generate file based on supplied config, defaulting to app-deploy.yaml
	yamlFile := filepath.Join(pdir, "app-service.yaml")
	if dryrun {
		Info.log("Skipping creation of yaml file with prefix: ", yamlFile)
		return yamlFile, nil
	}
	err = ioutil.WriteFile(yamlFile, yamlStr, 0666)
	if err != nil {
		return "", fmt.Errorf("Could not create the yaml file for the service %v", err)
	}
	return yamlFile, nil
}

//GenRouteYaml returns the file name of a generated K8S Service yaml
func GenRouteYaml(appName string, pdir string, port int, dryrun bool) (fileName string, err error) {
	type IngressPath struct {
		Path    string `yaml:"path"`
		Backend struct {
			ServiceName string `yaml:"serviceName"`
			ServicePort int    `yaml:"servicePort"`
		} `yaml:"backend"`
	}
	type IngressRule struct {
		Host string `yaml:"host"`
		HTTP struct {
			Paths []IngressPath `yaml:"paths"`
		} `yaml:"http"`
	}

	type Ingress struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec struct {
			Rules []IngressRule `yaml:"rules"`
		} `yaml:"spec"`
	}

	var ingress Ingress
	ingress.APIVersion = "extensions/v1beta1"
	ingress.Kind = "Ingress"
	ingress.Metadata.Name = fmt.Sprintf("%s-%s", appName, "ingress")

	ingress.Spec.Rules = make([]IngressRule, 1)
	ingress.Spec.Rules[0].Host = fmt.Sprintf("%s.%s.%s", appName, getK8sMasterIP(dryrun), "nip.io")
	ingress.Spec.Rules[0].HTTP.Paths = make([]IngressPath, 1)
	ingress.Spec.Rules[0].HTTP.Paths[0].Path = "/"
	ingress.Spec.Rules[0].HTTP.Paths[0].Backend.ServiceName = fmt.Sprintf("%s-%s", appName, "service")
	ingress.Spec.Rules[0].HTTP.Paths[0].Backend.ServicePort = port

	yamlStr, err := yaml.Marshal(&ingress)
	if err != nil {
		Error.log("Could not create the YAML string from Map. Exiting.")
		return "", err
	}
	Debug.logf("Generated YAML: \n%s\n", yamlStr)
	// Generate file based on supplied config, defaulting to app-deploy.yaml
	yamlFile := filepath.Join(pdir, "app-ingress.yaml")
	if dryrun {
		Info.log("Skipping creation of yaml file with prefix: ", yamlFile)
		return yamlFile, nil
	}
	err = ioutil.WriteFile(yamlFile, yamlStr, 0666)
	if err != nil {
		return "", fmt.Errorf("Could not create the yaml file for the route %v", err)
	}
	return yamlFile, nil
}

func getK8sMasterIP(dryrun bool) string {
	cmdParms := []string{"node", "--selector", "node-role.kubernetes.io/master", "-o", "jsonpath={.items[0].status.addresses[?(.type==\"InternalIP\")].address}"}
	ip, err := KubeGet(cmdParms, "", dryrun)
	if err == nil {
		return ip
	}
	Debug.log("Could not retrieve the master IP address - returning x.x.x.x: ", err)
	return "x.x.x.x"
}

func getIngressPort(config *RootCommandConfig) int {
	ports, err := getExposedPorts(config)

	knownHTTPPorts := []string{"80", "8080", "8008", "3000", "9080"}
	if err != nil {
		Debug.Log("Error trying to obtain the exposed ports: ", err)
		return 0
	}
	if len(ports) < 1 {
		Debug.log("Container doesn't expose any port - returning 0")
		return 0
	}
	iPort := 0
	for _, port := range ports {
		for _, knownPort := range knownHTTPPorts {
			if port == knownPort {
				iPort, err := strconv.Atoi(port)
				if err == nil {
					return iPort
				}
			}
		}
	}
	//If we haven't returned yet, there was no match
	//Pick the first port and return it
	Debug.Log("No known HTTP port detected, returning the first one on the list.")
	iPort, err = strconv.Atoi(ports[0])
	if err == nil {
		return iPort
	}
	Debug.Logf("Error converting port %s - returning 0: %v", ports[0], err)
	return 0
}

func getKNativeTemplate() string {
	yamltempl := `
apiVersion: serving.knative.dev/v1alpha1
kind: Service
metadata:
  name: test
spec:
  runLatest:
    configuration:
      revisionTemplate:
        spec:
          container:
            image: myimage
            imagePullPolicy: Always
            ports:
            - containerPort: 8080
`
	return yamltempl
}

// DockerTag tags a docker image
func DockerTag(imageToTag string, tag string, dryrun bool) error {
	Info.log("Tagging Docker image as ", tag)
	cmdName := "docker"
	cmdArgs := []string{"image", "tag", imageToTag, tag}
	if dryrun {
		Info.log("Dry run - skipping execution of: ", cmdName, " ", strings.Join(cmdArgs, " "))
		return nil
	}
	tagCmd := exec.Command(cmdName, cmdArgs...)
	tagOut, tagErr := tagCmd.Output()
	if tagErr != nil {
		Error.log("Could not inspect the image: ", tagErr, " ", string(tagOut[:]))
		return tagErr
	}
	Debug.log("Docker tag command output: ", string(tagOut[:]))
	return nil
}

//DockerPush pushes a docker image to a docker registry (assumes that the user has done docker login)
func DockerPush(imageToPush string, dryrun bool) error {
	Info.log("Pushing docker image ", imageToPush)
	cmdName := "docker"
	cmdArgs := []string{"push", imageToPush}
	if dryrun {
		Info.log("Dry run - skipping execution of: ", cmdName, " ", strings.Join(cmdArgs, " "))
		return nil
	}
	pushCmd := exec.Command(cmdName, cmdArgs...)
	pushOut, pushErr := pushCmd.Output()
	if pushErr != nil {
		Error.log("Could not push the image: ", pushErr, " ", string(pushOut[:]))
		return pushErr
	}
	Debug.log("Docker push command output: ", string(pushOut[:]))
	return nil
}

// DockerRunBashCmd issues a shell command in a docker image, overriding its entrypoint
func DockerRunBashCmd(options []string, image string, bashCmd string, config *RootCommandConfig) (cmdOutput string, err error) {
	cmdName := "docker"
	var cmdArgs []string
	pullErrs := pullImage(image, config)
	if pullErrs != nil {
		return "", pullErrs
	}
	if len(options) >= 0 {
		cmdArgs = append([]string{"run"}, options...)
	} else {
		cmdArgs = []string{"run"}
	}
	cmdArgs = append(cmdArgs, "--entrypoint", "/bin/bash", image, "-c", bashCmd)
	Info.log("Running command: ", cmdName, " ", strings.Join(cmdArgs, " "))
	dockerCmd := exec.Command(cmdName, cmdArgs...)
	dockerOutBytes, err := dockerCmd.Output()
	if err != nil {
		Error.log("Could not run the docker image: ", err)
		return "", err
	}
	dockerOut := strings.TrimSpace(string(dockerOutBytes))
	return dockerOut, nil
}

//KubeGet issues kubectl get <arg>
func KubeGet(args []string, namespace string, dryrun bool) (string, error) {
	Info.log("Attempting to get resource from Kubernetes ...")
	kcmd := "kubectl"
	kargs := []string{"get"}
	kargs = append(kargs, args...)
	if namespace != "" {
		kargs = append(kargs, "--namespace", namespace)
	}

	if dryrun {
		Info.log("Dry run - skipping execution of: ", kcmd, " ", strings.Join(kargs, " "))
		return "", nil
	}
	Info.log("Running command: ", kcmd, " ", strings.Join(kargs, " "))
	execCmd := exec.Command(kcmd, kargs...)
	kout, kerr := execCmd.Output()
	if kerr != nil {
		return "", errors.Errorf("kubectl get failed: %s", string(kout[:]))
	}
	return string(kout[:]), nil
}

//KubeApply issues kubectl apply -f <filename>
func KubeApply(fileToApply string, namespace string, dryrun bool) error {
	Info.log("Attempting to apply resource in Kubernetes ...")
	kcmd := "kubectl"
	kargs := []string{"apply", "-f", fileToApply}
	if namespace != "" {
		kargs = append(kargs, "--namespace", namespace)
	}

	if dryrun {
		Info.log("Dry run - skipping execution of: ", kcmd, " ", strings.Join(kargs, " "))
		return nil
	}
	Info.log("Running command: ", kcmd, " ", strings.Join(kargs, " "))
	execCmd := exec.Command(kcmd, kargs...)
	kout, kerr := execCmd.Output()
	if kerr != nil {
		Error.log("kubectl apply failed: ", kerr, " ", string(kout[:]))
		return kerr
	}
	Debug.log("kubectl apply success: ", string(kout[:]))
	return nil
}

//KubeDelete issues kubectl delete -f <filename>
func KubeDelete(fileToApply string, namespace string, dryrun bool) error {
	Info.log("Attempting to delete resource from Kubernetes...")
	kcmd := "kubectl"
	kargs := []string{"delete", "-f", fileToApply}
	if namespace != "" {
		kargs = append(kargs, "--namespace", namespace)
	}

	if dryrun {
		Info.log("Dry run - skipping execution of: ", kcmd, " ", strings.Join(kargs, " "))
		return nil
	}
	Info.log("Running command: ", kcmd, " ", strings.Join(kargs, " "))
	execCmd := exec.Command(kcmd, kargs...)
	var stderr bytes.Buffer
	execCmd.Stderr = &stderr
	kout, kerr := execCmd.Output()
	if kerr != nil {
		errorText := strings.Trim(stderr.String(), "\n")
		Error.log(errorText)
		Error.log("kubectl delete failed: ", kerr)
		return errors.Errorf("kubectl delete failed: %v %s", kerr, errorText)
	}
	Debug.log("kubectl delete success: ", string(kout[:]))
	return nil
}

//KubeGetNodePortURL kubectl get svc <service> -o jsonpath=http://{.status.loadBalancer.ingress[0].hostname}:{.spec.ports[0].nodePort} and prints the return URL
func KubeGetNodePortURL(service string, namespace string, dryrun bool) (url string, err error) {
	kargs := append([]string{"svc"}, service)
	kargs = append(kargs, "-o", "jsonpath=http://{.status.loadBalancer.ingress[0].hostname}:{.spec.ports[0].nodePort}")
	out, err := KubeGet(kargs, namespace, dryrun)
	// Performing the kubectl apply
	if err != nil {
		return "", errors.Errorf("Failed to find deployed service IP and Port: %s", err)
	}
	return out, nil
}

//KubeGetRouteURL issues kubectl get svc <service> -o jsonpath=http://{.status.loadBalancer.ingress[0].hostname}:{.spec.ports[0].nodePort} and prints the return URL
func KubeGetRouteURL(service string, namespace string, dryrun bool) (url string, err error) {
	kargs := append([]string{"route"}, service)
	kargs = append(kargs, "-o", "jsonpath={.status.ingress[0].host}")
	out, err := KubeGet(kargs, namespace, dryrun)
	// Performing the kubectl apply
	if err != nil {
		return "", errors.Errorf("Failed to find deployed service IP and Port: %s", err)
	}
	return out, nil
}

//KubeGetKnativeURL issues kubectl get rt <service> -o jsonpath="{.status.url}" and prints the return URL
func KubeGetKnativeURL(service string, namespace string, dryrun bool) (url string, err error) {
	kcmd := "kubectl"
	kargs := append([]string{"get", "rt"}, service)
	kargs = append(kargs, "-o", "jsonpath=\"{.status.url}\"")
	if namespace != "" {
		kargs = append(kargs, "--namespace", namespace)
	}

	if dryrun {
		Info.log("Dry run - skipping execution of: ", kcmd, " ", strings.Join(kargs, " "))
		return "", nil
	}
	Info.log("Running command: ", kcmd, " ", strings.Join(kargs, " "))
	execCmd := exec.Command(kcmd, kargs...)
	kout, kerr := execCmd.Output()
	if kerr != nil {
		return "", errors.Errorf("kubectl get failed: %s", string(kout[:]))
	}
	return string(kout[:]), nil
}

//KubeGetDeploymentURL searches for an exposed hostname and port for the deployed service
func KubeGetDeploymentURL(service string, namespace string, dryrun bool) (url string, err error) {
	url, err = KubeGetKnativeURL(service, namespace, dryrun)
	if err == nil {
		return url, nil
	}
	url, err = KubeGetRouteURL(service, namespace, dryrun)
	if err == nil {
		return url, nil
	}
	url, err = KubeGetNodePortURL(service, namespace, dryrun)
	if err == nil {
		return url, nil
	}
	Error.log("Failed to get deployment hostname and port: ", err)
	return "", err
}

//pullCmd
// enable extract to use `buildah` sequences for image extraction.
// Pull the given docker image
func pullCmd(imageToPull string, buildah bool, dryrun bool) error {
	cmdName := "docker"
	if buildah {
		cmdName = "buildah"
	}
	pullArgs := []string{"pull", imageToPull}
	if dryrun {
		Info.log("Dry run - skipping execution of: ", cmdName, " ", strings.Join(pullArgs, " "))
		return nil
	}
	Info.log("Pulling docker image ", imageToPull)
	err := execAndWaitReturnErr(cmdName, pullArgs, Info, dryrun)
	if err != nil {
		Warning.log("Docker image pull failed: ", err)
		return err
	}
	return nil
}

func checkDockerImageExistsLocally(imageToPull string) bool {
	cmdName := "docker"
	cmdArgs := []string{"image", "ls", "-q", imageToPull}
	imagelsCmd := exec.Command(cmdName, cmdArgs...)
	imagelsOut, imagelsErr := imagelsCmd.Output()
	imagelsOutStr := strings.TrimSpace(string(imagelsOut))
	Debug.log("Docker image ls command output: ", imagelsOutStr)

	if imagelsErr != nil {
		Warning.log("Could not run docker image ls -q for the image: ", imageToPull, " error: ", imagelsErr, " Check to make sure docker is available.")
		return false
	}
	if imagelsOutStr != "" {
		return true
	}
	return false
}

//pullImage
// pulls buildah / docker image, if APPSODY_PULL_POLICY set to IFNOTPRESENT
//it checks for image in local repo and pulls if not in the repo
func pullImage(imageToPull string, config *RootCommandConfig) error {
	if config.imagePulled == nil {
		config.imagePulled = make(map[string]bool)
	}
	Debug.logf("%s image pulled status: %t", imageToPull, config.imagePulled[imageToPull])
	if config.imagePulled[imageToPull] {
		Debug.log("Image has been pulled already: ", imageToPull)
		return nil
	}
	config.imagePulled[imageToPull] = true

	localImageFound := false
	pullPolicyAlways := true
	pullPolicy := os.Getenv("APPSODY_PULL_POLICY") // Always or IfNotPresent
	if pullPolicy == "" || strings.ToUpper(pullPolicy) == "ALWAYS" {
		Debug.log("Pull policy Always")
	} else if strings.ToUpper(pullPolicy) == "IFNOTPRESENT" {
		Debug.log("Pull policy IfNotPresent, checking for local image")
		pullPolicyAlways = false
	}
	if !pullPolicyAlways {
		localImageFound = checkDockerImageExistsLocally(imageToPull)
	}

	if pullPolicyAlways || (!pullPolicyAlways && !localImageFound) {
		err := pullCmd(imageToPull, config.Buildah, config.Dryrun)
		if err != nil {
			if pullPolicyAlways {
				localImageFound = checkDockerImageExistsLocally(imageToPull)
			}
			if !localImageFound {
				return errors.Errorf("Could not find the image either in docker hub or locally: %s", imageToPull)

			}
		}
	}
	if localImageFound {
		Info.log("Using local cache for image ", imageToPull)
	}
	return nil
}

func execAndListenWithWorkDirReturnErr(command string, args []string, logger appsodylogger, workdir string, dryrun bool) (*exec.Cmd, error) {
	var execCmd *exec.Cmd
	var err error
	if dryrun {
		Info.log("Dry Run - Skipping command: ", command, " ", strings.Join(args, " "))
	} else {
		Info.log("Running command: ", command, " ", strings.Join(args, " "))
		execCmd = exec.Command(command, args...)
		if workdir != "" {
			execCmd.Dir = workdir
		}
		cmdReader, err := execCmd.StdoutPipe()
		if err != nil {
			Error.log("Error creating StdoutPipe for Cmd ", err)
			return nil, err
		}

		errReader, err := execCmd.StderrPipe()
		if err != nil {
			Error.log("Error creating StderrPipe for Cmd ", err)
			return nil, err
		}

		outScanner := bufio.NewScanner(cmdReader)
		outScanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		go func() {
			for outScanner.Scan() {
				logger.log(outScanner.Text())
			}
		}()

		errScanner := bufio.NewScanner(errReader)
		errScanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		go func() {
			for errScanner.Scan() {
				logger.log(errScanner.Text())
			}
		}()

		err = execCmd.Start()
		if err != nil {
			Debug.log("Error running ", command, " command: ", errScanner.Text(), err)
			return nil, err
		}
	}
	return execCmd, err
}

func execAndWaitReturnErr(command string, args []string, logger appsodylogger, dryrun bool) error {
	return execAndWaitWithWorkDirReturnErr(command, args, logger, "", dryrun)
}
func execAndWaitWithWorkDirReturnErr(command string, args []string, logger appsodylogger, workdir string, dryrun bool) error {
	var err error
	var execCmd *exec.Cmd
	if dryrun {
		Info.log("Dry Run - Skipping command: ", command, " ", strings.Join(args, " "))
	} else {
		execCmd, err = execAndListenWithWorkDirReturnErr(command, args, logger, workdir, dryrun)
		if err != nil {
			return err
		}
		err = execCmd.Wait()
		if err != nil {
			return err
		}
	}
	return err
}

func createChecksumHash(fileName string) (hash.Hash, error) {
	Debug.log("Checksum oldFile", fileName)
	newFile, err := os.Open(fileName)
	if err != nil {
		return nil, errors.Errorf("File open failed for %s controller binary: %v", fileName, err)

	}
	defer newFile.Close()

	computedSha256 := sha256.New()
	if _, err := io.Copy(computedSha256, newFile); err != nil {
		return nil, errors.Errorf("sha256 copy failed for %s controller binary %v", fileName, err)
	}
	return computedSha256, nil
}

func checksum256TestFile(newFileName string, oldFileName string) (bool, error) {
	var checkValue bool

	oldSha256, errOld := createChecksumHash(oldFileName)
	if errOld != nil {
		return false, errOld
	}
	newSha256, errNew := createChecksumHash(newFileName)
	if errNew != nil {
		return false, errNew
	}
	Debug.logf("%x\n", oldSha256.Sum(nil))
	Debug.logf("%x\n", newSha256.Sum(nil))
	checkValue = bytes.Equal(oldSha256.Sum(nil), newSha256.Sum(nil))

	Debug.log("Checksum returned: ", checkValue)

	return checkValue, nil
}

func getLatestVersion() string {
	var version string
	Debug.log("Getting latest version ", LatestVersionURL)
	resp, err := http.Get(LatestVersionURL)
	if err != nil {
		Warning.log("Unable to check the most recent version of Appsody in GitHub.... continuing.")
	} else {
		url := resp.Request.URL.String()
		r, _ := regexp.Compile(`[\d]+\.[\d]+\.[\d]+$`)

		version = r.FindString(url)
	}
	Debug.log("Got version ", version)
	return version
}

func doVersionCheck(config *RootCommandConfig) {
	var latest = getLatestVersion()
	var currentTime = time.Now().Format("2006-01-02 15:04:05 -0700 MST")

	if latest != "" && VERSION != "vlatest" && VERSION != latest {
		switch os := runtime.GOOS; os {
		case "darwin":
			Info.logf("\n*\n*\n*\n\nA new CLI update is available.\nPlease run `brew upgrade appsody` to upgrade from %s --> %s.\n\n*\n*\n*", VERSION, latest)
		default:
			Info.logf("\n*\n*\n*\n\nA new CLI update is available.\nPlease go to https://appsody.dev/docs/getting-started/installation#upgrading-appsody and upgrade from %s --> %s.\n\n*\n*\n*", VERSION, latest)
		}
	}

	config.CliConfig.Set("lastversioncheck", currentTime)
	if err := config.CliConfig.WriteConfig(); err != nil {
		Error.logf("Writing default config file %s", err)

	}
}

func getLastCheckTime(config *RootCommandConfig) string {
	return config.CliConfig.GetString("lastversioncheck")
}

func checkTime(config *RootCommandConfig) {
	var lastCheckTime = getLastCheckTime(config)

	lastTime, err := time.Parse("2006-01-02 15:04:05 -0700 MST", lastCheckTime)
	if err != nil {
		Debug.logf("Could not parse the config file's lastversioncheck: %v. Continuing with a new version check...", err)
		doVersionCheck(config)
	} else if time.Since(lastTime).Hours() > 24 {
		doVersionCheck(config)
	}

}

// TEMPORARY CODE: sets the old v1 index to point to the new v2 index (latest)
// this code should be removed when we think everyone is using the latest index.
func setNewIndexURL(config *RootCommandConfig) {

	var repoFile = getRepoFileLocation(config)
	var oldIndexURL = "https://raw.githubusercontent.com/appsody/stacks/master/index.yaml"
	var newIndexURL = "https://github.com/appsody/stacks/releases/latest/download/incubator-index.yaml"

	data, err := ioutil.ReadFile(repoFile)
	if err != nil {
		Warning.log("Unable to read repository file")
	}

	replaceURL := bytes.Replace(data, []byte(oldIndexURL), []byte(newIndexURL), -1)

	if err = ioutil.WriteFile(repoFile, replaceURL, 0644); err != nil {
		Warning.log(err)
	}
}

func IsEmptyDir(name string) bool {
	f, err := os.Open(name)

	if err != nil {
		return true
	}
	defer f.Close()

	_, err = f.Readdirnames(1)

	return err == io.EOF
}

// tar and zip a directory into .tar.gz
func Targz(source, target string) error {
	filename := filepath.Base(source)
	Info.log("source is: ", source)
	Info.log("filename is: ", filename)
	Info.log("target is: ", target)
	target = target + filename + ".tar.gz"
	//target = filepath.Join(target, fmt.Sprintf("%s.tar.gz", filename))
	Info.log("new target is: ", target)
	tarfile, err := os.Create(target)
	if err != nil {
		return err
	}
	defer tarfile.Close()

	var fileWriter io.WriteCloser = tarfile
	fileWriter = gzip.NewWriter(tarfile)
	defer fileWriter.Close()

	tarball := tar.NewWriter(fileWriter)
	defer tarball.Close()

	info, err := os.Stat(source)
	if err != nil {
		return err
	}

	var baseDir string
	if info.IsDir() {
		baseDir = filepath.Base(source)
	}

	return filepath.Walk(source,
		func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			header, err := tar.FileInfoHeader(info, info.Name())
			if err != nil {
				return err
			}

			if baseDir != "" {
				header.Name = "." + strings.TrimPrefix(path, source)
			}

			if err := tarball.WriteHeader(header); err != nil {
				return err
			}

			if info.IsDir() {
				return nil
			}

			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()
			_, err = io.Copy(tarball, file)
			return err
		})
}
