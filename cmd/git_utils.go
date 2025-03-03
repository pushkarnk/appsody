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
	"encoding/json"
	"os/exec"
	"runtime"
	"strings"

	"github.com/pkg/errors"
)

type CommitInfo struct {
	Author string
	SHA    string
	Date   string
	URL    string
}

type GitInfo struct {
	Branch    string
	Upstream  string
	RemoteURL string

	ChangesMade bool
	Commit      CommitInfo
}

const trimChars = "' \r\n"

func stringBefore(value string, searchValue string) string {
	// Get substring before a string.

	gitURLElements := strings.Split(value, searchValue)
	if len(gitURLElements) == 0 {
		return ""
	}
	return gitURLElements[0]

}

func stringAfter(value string, searchValue string) string {
	// Get substring after a string.
	position := strings.LastIndex(value, searchValue)
	if position == -1 {
		return ""
	}
	adjustedPosition := position + len(searchValue)
	if adjustedPosition >= len(value) {
		return ""
	}
	return value[adjustedPosition:]
}
func stringBetween(value string, pre string, post string) string {
	// Get substring between two strings.
	positionBegin := strings.Index(value, pre)
	if positionBegin == -1 {
		return ""
	}
	positionEnd := strings.Index(value, post)
	if positionEnd == -1 {
		return ""
	}
	positionBeginAdjusted := positionBegin + len(pre)
	if positionBeginAdjusted >= positionEnd {
		return ""
	}
	return value[positionBeginAdjusted:positionEnd]
}

//RunGitFindBranc issues git status
func GetGitInfo(dryrun bool) (GitInfo, error) {
	var gitInfo GitInfo
	version, vErr := RunGitVersion(false)
	if vErr != nil {
		return gitInfo, vErr
	}
	if version == "" {
		return gitInfo, errors.Errorf("git does not appear to be available")
	}

	Debug.log("git version: ", version)

	kargs := []string{"status", "-sb"}

	output, gitErr := RunGit(kargs, dryrun)
	if gitErr != nil {
		return gitInfo, gitErr
	}

	lineSeparator := "\n"
	if runtime.GOOS == "windows" {
		lineSeparator = "\r\n"
	}
	output = strings.Trim(output, trimChars)
	outputLines := strings.Split(output, lineSeparator)

	const noCommits = "## No commits yet on "
	const branchPrefix = "## "
	const branchSeparatorString = "..."

	value := strings.Trim(outputLines[0], trimChars)

	if strings.HasPrefix(value, branchPrefix) {
		if strings.Contains(value, branchSeparatorString) {
			gitInfo.Branch = strings.Trim(stringBetween(value, branchPrefix, branchSeparatorString), trimChars)
			gitInfo.Upstream = strings.Trim(stringAfter(value, branchSeparatorString), trimChars)
		} else {
			gitInfo.Branch = strings.Trim(stringAfter(value, branchPrefix), trimChars)
		}

	}
	if strings.Contains(value, noCommits) {
		gitInfo.Branch = stringAfter(value, noCommits)
	}
	changesMade := false
	outputLength := len(outputLines)

	if outputLength > 1 {
		changesMade = true

	}
	gitInfo.ChangesMade = changesMade

	if gitInfo.Upstream != "" {
		gitInfo.RemoteURL, gitErr = RunGitConfigLocalRemoteOriginURL(gitInfo.Upstream, dryrun)
		if gitErr != nil {
			Info.Logf("Could not construct repository URL %v", gitErr)
		}

	} else {
		Info.log("Unable to determine origin to compute repository URL")
	}

	gitInfo.Commit, gitErr = RunGitGetLastCommit(gitInfo.RemoteURL, dryrun)
	if gitErr != nil {
		Info.log("Received error getting current commit: ", gitErr)

	}

	return gitInfo, nil
}

//RunGitConfigLocalRemoteOriginURL
func RunGitConfigLocalRemoteOriginURL(upstream string, dryrun bool) (string, error) {
	Info.log("Attempting to perform git config --local remote.<origin>.url  ...")

	upstreamStart := strings.Split(upstream, "/")[0]
	kargs := []string{"config", "--local", "remote." + upstreamStart + ".url"}
	return RunGit(kargs, dryrun)
}

//RunGitLog issues git log
func RunGitGetLastCommit(URL string, dryrun bool) (CommitInfo, error) {
	//git log -n 1 --pretty=format:"{"author":"%cn","sha":"%h","date":"%cd”}”
	kargs := []string{"log", "-n", "1", "--pretty=format:'{\"author\":\"%cn\",\"sha\":\"%H\",\"date\":\"%cd\"}'"}
	kargs = append(kargs)
	var commitInfo CommitInfo
	commitStringInfo, gitErr := RunGit(kargs, dryrun)
	if gitErr != nil {
		return commitInfo, gitErr
	}
	err := json.Unmarshal([]byte(strings.Trim(commitStringInfo, trimChars)), &commitInfo)
	if err != nil {
		return commitInfo, errors.Errorf("JSON Unmarshall error: %v", err)
	}
	if URL != "" {
		commitInfo.URL = stringBefore(URL, ".git") + "/commit/" + commitInfo.SHA
	}
	return commitInfo, nil

}

//RunGitVersion
func RunGitVersion(dryrun bool) (string, error) {
	kargs := []string{"version"}
	kargs = append(kargs)
	versionInfo, gitErr := RunGit(kargs, dryrun)
	if gitErr != nil {
		return "", gitErr
	}
	return strings.Trim(versionInfo, trimChars), nil
}

//RunGit runs a generic git
func RunGit(kargs []string, dryrun bool) (string, error) {
	kcmd := "git"
	if dryrun {
		Info.log("Dry run - skipping execution of: ", kcmd, " ", strings.Join(kargs, " "))
		return "", nil
	}
	Info.log("Running git command: ", kcmd, " ", strings.Join(kargs, " "))
	execCmd := exec.Command(kcmd, kargs...)
	kout, kerr := execCmd.Output()

	if kerr != nil {
		return "", errors.Errorf("git command failed: %s", string(kout[:]))
	}
	Debug.log("Command successful...")
	return string(kout[:]), nil
}
