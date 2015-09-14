/*-
 * Copyright 2015 Grammarly, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package build

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"rocker/dockerclient"
	"rocker/imagename"
	"rocker/parser"

	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/fsouza/go-dockerclient"
)

var (
	captureImageID = regexp.MustCompile("Successfully built ([a-z0-9]{12})")
)

func (builder *Builder) checkDockerignore() (err error) {
	ignoreLines := []string{
		".dockerignore",
		builder.getTmpPrefix() + "*",
		builder.rockerfileRelativePath(),
	}
	dockerignoreFile := path.Join(builder.ContextDir, ".dockerignore")

	// everything is easy, we just need to create one
	if _, err := os.Stat(dockerignoreFile); os.IsNotExist(err) {
		fmt.Fprintf(builder.OutStream, "[Rocker] Create .dockerignore in context directory\n")
		newLines := append([]string{
			"# This file is automatically generated by Rocker, please keep it",
		}, ignoreLines...)
		return ioutil.WriteFile(dockerignoreFile, []byte(strings.Join(newLines, "\n")+"\n"), 0644)
	}

	// more difficult, find missing lines
	file, err := os.Open(dockerignoreFile)
	if err != nil {
		return err
	}
	defer file.Close()

	// read current .dockerignore and filter those ignoreLines which are already there
	scanner := bufio.NewScanner(file)
	newLines := []string{}
	for scanner.Scan() {
		currentLine := scanner.Text()
		newLines = append(newLines, currentLine)
		if currentLine == ".git" {
			builder.gitIgnored = true
		}
		for i, ignoreLine := range ignoreLines {
			if ignoreLine == currentLine {
				ignoreLines = append(ignoreLines[:i], ignoreLines[i+1:]...)
				break
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	// if we have still something to add - do it
	if len(ignoreLines) > 0 {
		newLines = append(newLines, ignoreLines...)
		fmt.Fprintf(builder.OutStream, "[Rocker] Add %d lines to .dockerignore\n", len(ignoreLines))
		return ioutil.WriteFile(dockerignoreFile, []byte(strings.Join(newLines, "\n")+"\n"), 0644)
	}

	return nil
}

func (builder *Builder) runDockerfile() (err error) {
	if len(builder.dockerfile.Children) == 0 {
		return nil
	}

	// HACK: skip if all we have is "FROM scratch", we need to do something
	// to produce actual layer with ID, so create dummy LABEL layer
	// maybe there is a better solution, but keep this for a while
	if len(builder.dockerfile.Children) == 1 &&
		builder.dockerfile.Children[0].Value == "from" &&
		builder.dockerfile.Children[0].Next.Value == "scratch" {

		builder.dockerfile.Children = append(builder.dockerfile.Children, &parser.Node{
			Value: "label",
			Next: &parser.Node{
				Value: "ROCKER_SCRATCH=1",
			},
		})
	}

	// missing from, use latest image sha
	if builder.dockerfile.Children[0].Value != "from" {
		if builder.imageID == "" {
			return fmt.Errorf("Missing initial FROM instruction")
		}
		fromNode := &parser.Node{
			Value: "from",
			Next: &parser.Node{
				Value: builder.imageID,
			},
		}
		builder.dockerfile.Children = append([]*parser.Node{fromNode}, builder.dockerfile.Children...)
	}

	// Write Dockerfile to a context
	dockerfileName := builder.dockerfileName()
	dockerfilePath := path.Join(builder.ContextDir, dockerfileName)

	dockerfileContent, err := RockerfileAstToString(builder.dockerfile)
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile(dockerfilePath, []byte(dockerfileContent), 0644); err != nil {
		return err
	}
	defer os.Remove(dockerfilePath)

	// TODO: here we can make a hint to a user, if the context directory is very large,
	// suggest to add some stuff to .dockerignore, etc

	pipeReader, pipeWriter := io.Pipe()

	var buf bytes.Buffer
	outStream := io.MultiWriter(pipeWriter, &buf)

	// TODO: consider ForceRmTmpContainer: true
	opts := docker.BuildImageOptions{
		Dockerfile:    dockerfileName,
		OutputStream:  outStream,
		ContextDir:    builder.ContextDir,
		NoCache:       !builder.UtilizeCache,
		Auth:          *builder.Auth,
		RawJSONStream: true,
	}

	errch := make(chan error)

	go func() {
		err := builder.Docker.BuildImage(opts)

		if err := pipeWriter.Close(); err != nil {
			fmt.Fprintf(builder.OutStream, "pipeWriter.Close() err: %s\n", err)
		}

		errch <- err
	}()

	if err := jsonmessage.DisplayJSONMessagesStream(pipeReader, builder.OutStream, builder.fdOut, builder.isTerminalOut); err != nil {
		return fmt.Errorf("Failed to process json stream error: %s", err)
	}

	if err := <-errch; err != nil {
		return fmt.Errorf("Failed to build image: %s", err)
	}

	// It is the best way to have built image id so far
	// The other option would be to tag the image, and then remove the tag
	// http://stackoverflow.com/questions/19776308/get-image-id-from-image-created-via-remote-api
	matches := captureImageID.FindStringSubmatch(buf.String())
	if len(matches) == 0 {
		return fmt.Errorf("Couldn't find image id out of docker build output")
	}
	imageID := matches[1]

	// Retrieve image id
	image, err := builder.Docker.InspectImage(imageID)
	if err != nil {
		// fix go-dockerclient non descriptive error
		if err.Error() == "no such image" {
			err = fmt.Errorf("No such image: %s", imageID)
		}
		return err
	}

	builder.imageID = image.ID
	builder.Config = image.Config

	// clean it up
	builder.dockerfile = &parser.Node{}

	return nil
}

func (builder *Builder) addLabels(labels map[string]string) {
	if builder.Config.Labels == nil {
		builder.Config.Labels = map[string]string{}
	}
	for k, v := range labels {
		builder.Config.Labels[k] = v
	}
}

func (builder *Builder) temporaryCmd(cmd []string) func() {
	origCmd := builder.Config.Cmd
	builder.Config.Cmd = cmd
	return func() {
		builder.Config.Cmd = origCmd
	}
}

func (builder *Builder) temporaryConfig(fn func()) func() {
	// actually copy the whole config
	origConfig := *builder.Config
	fn()
	return func() {
		builder.Config = &origConfig
	}
}

func (builder *Builder) probeCache() (bool, error) {
	if !builder.UtilizeCache || builder.cacheBusted {
		return false, nil
	}

	cache, err := builder.imageGetCached(builder.imageID, builder.Config)
	if err != nil {
		return false, err
	}
	if cache == nil {
		builder.cacheBusted = true
		return false, nil
	}

	fmt.Fprintf(builder.OutStream, "[Rocker]  ---> Using cache\n")

	builder.imageID = cache.ID
	return true, nil
}

func (builder *Builder) imageGetCached(imageID string, config *docker.Config) (*docker.Image, error) {
	// Retrieve all images
	images, err := builder.Docker.ListImages(docker.ListImagesOptions{All: true})
	if err != nil {
		return nil, err
	}

	var siblings []string
	for _, img := range images {
		if img.ParentID != imageID {
			continue
		}
		siblings = append(siblings, img.ID)
	}

	// Loop on the children of the given image and check the config
	var match *docker.Image

	if len(siblings) == 0 {
		return match, nil
	}

	// TODO: ensure goroutines die if return abnormally

	ch := make(chan *docker.Image)
	errch := make(chan error)
	numResponses := 0

	for _, siblingID := range siblings {
		go func(siblingID string) {
			image, err := builder.Docker.InspectImage(siblingID)
			if err != nil {
				errch <- err
				return
			}
			ch <- image
		}(siblingID)
	}

	for {
		select {
		case image := <-ch:
			if CompareConfigs(&image.ContainerConfig, config) {
				if match == nil || match.Created.Before(image.Created) {
					match = image
				}
			}

			numResponses++

			if len(siblings) == numResponses {
				return match, nil
			}

		case err := <-errch:
			return nil, err

		case <-time.After(10 * time.Second):
			// TODO: return "cache didn't hit"?
			return nil, fmt.Errorf("Timeout while fetching cached images")
		}
	}
}

func (builder *Builder) getContextMountSrc(sourcePath string) (string, error) {
	// TODO: refactor to use util.ResolvePath() ?
	if !filepath.IsAbs(sourcePath) {
		sourcePath = filepath.Join(builder.ContextDir, sourcePath)
	}
	sourcePath = filepath.Clean(sourcePath)

	return dockerclient.ResolveHostPath(sourcePath, builder.Docker)
}

func (builder *Builder) ensureImage(imageName string, purpose string) error {
	_, err := builder.Docker.InspectImage(imageName)
	if err != nil && err.Error() == "no such image" {
		fmt.Fprintf(builder.OutStream, "[Rocker] Pulling image: %s for %s\n", imageName, purpose)

		image := imagename.NewFromString(imageName)

		pipeReader, pipeWriter := io.Pipe()

		pullOpts := docker.PullImageOptions{
			Repository:    image.NameWithRegistry(),
			Registry:      image.Registry,
			Tag:           image.GetTag(),
			OutputStream:  pipeWriter,
			RawJSONStream: true,
		}

		errch := make(chan error)

		go func() {
			err := builder.Docker.PullImage(pullOpts, *builder.Auth)

			if err := pipeWriter.Close(); err != nil {
				fmt.Fprintf(builder.OutStream, "pipeWriter.Close() err: %s\n", err)
			}

			errch <- err
		}()

		if err := jsonmessage.DisplayJSONMessagesStream(pipeReader, builder.OutStream, builder.fdOut, builder.isTerminalOut); err != nil {
			return fmt.Errorf("Failed to process json stream for image: %s, error: %s", image, err)
		}

		if err := <-errch; err != nil {
			return fmt.Errorf("Failed to pull image: %s, error: %s", image, err)
		}
	} else if err != nil {
		return err
	}
	return nil
}

func (builder *Builder) pushImage(image imagename.ImageName) error {
	pipeReader, pipeWriter := io.Pipe()
	errch := make(chan error)

	go func() {
		err := builder.Docker.PushImage(docker.PushImageOptions{
			Name:          image.NameWithRegistry(),
			Tag:           image.GetTag(),
			Registry:      image.Registry,
			OutputStream:  pipeWriter,
			RawJSONStream: true,
		}, *builder.Auth)

		if err := pipeWriter.Close(); err != nil {
			fmt.Fprintf(builder.OutStream, "pipeWriter.Close() err: %s\n", err)
		}

		errch <- err
	}()

	if err := jsonmessage.DisplayJSONMessagesStream(pipeReader, builder.OutStream, builder.fdOut, builder.isTerminalOut); err != nil {
		return fmt.Errorf("Failed to process json stream for image: %s, error: %s", image, err)
	}

	if err := <-errch; err != nil {
		return fmt.Errorf("Failed to push image: %s, error: %s", image, err)
	}

	return nil
}

func (builder *Builder) makeExportsContainer() (string, error) {
	if builder.exportsContainerID != "" {
		return builder.exportsContainerID, nil
	}
	exportsContainerName := builder.exportsContainerName()

	containerConfig := &docker.Config{
		Image: rsyncImage,
		Volumes: map[string]struct{}{
			"/opt/rsync/bin": struct{}{},
			exportsVolume:    struct{}{},
		},
		Labels: map[string]string{
			"Rockerfile": builder.Rockerfile,
			"ImageId":    builder.imageID,
		},
	}

	container, err := builder.ensureContainer(exportsContainerName, containerConfig, "exports")
	if err != nil {
		return "", err
	}

	builder.exportsContainerID = container.ID

	return container.ID, nil
}

func (builder *Builder) getMountContainerIds() []string {
	containerIds := make(map[string]struct{})
	for _, mount := range builder.mounts {
		if mount.containerID != "" {
			containerIds[mount.containerID] = struct{}{}
		}
	}
	result := []string{}
	for containerID := range containerIds {
		result = append(result, containerID)
	}
	return result
}

func (builder *Builder) getAllMountContainerIds() []string {
	containerIds := make(map[string]struct{})
	for _, mount := range builder.allMounts {
		if mount.containerID != "" {
			containerIds[mount.containerID] = struct{}{}
		}
	}
	result := []string{}
	for containerID := range containerIds {
		result = append(result, containerID)
	}
	return result
}

func (builder *Builder) getBinds() []string {
	var result []string
	for _, mount := range builder.mounts {
		if mount.containerID == "" {
			result = append(result, mount.src+":"+mount.dest)
		}
	}
	return result
}
