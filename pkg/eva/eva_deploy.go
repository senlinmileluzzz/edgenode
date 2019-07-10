// Copyright 2019 Intel Corporation and Smart-Edge.com, Inc. All rights reserved
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

package eva

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"

	"math"
	"regexp"
	"strings"
	"time"

	"github.com/golang/protobuf/ptypes/empty"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	libvirtxml "github.com/libvirt/libvirt-go-xml"

	"github.com/pkg/errors"
	metadata "github.com/smartedgemec/appliance-ce/pkg/app-metadata"
	pb "github.com/smartedgemec/appliance-ce/pkg/eva/pb"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	libvirt "github.com/libvirt/libvirt-go"
)

// DeploySrv describes deplyment
type DeploySrv struct {
	cfg  *Config
	meta *metadata.AppMetadata
}

var httpMatcher = regexp.MustCompile("^http://.")
var httpsMatcher = regexp.MustCompile("^https://.")

func downloadImage(url string, target string, timeout time.Duration) error {
	var input io.Reader

	if httpMatcher.MatchString(url) {
		return fmt.Errorf("HTTP image path unsupported as insecure, " +
			"please use HTTPS")
	} else if httpsMatcher.MatchString(url) {
		client := &http.Client{Timeout: timeout}
		resp, err := client.Get(url)
		if err != nil {
			return err
		}

		defer func() {
			if err1 := resp.Body.Close(); err1 != nil {
				log.Errf("Failed to close body reader from %s: %v", url, err1)
			}
		}()

		if resp.StatusCode != 200 {
			return fmt.Errorf("unexpected HTTP code %v returned",
				resp.StatusCode)
		}

		input = resp.Body
	} else {
		file, err := os.Open(filepath.Clean(url))
		if err != nil {
			return err
		}
		defer func() {
			if err1 := file.Close(); err1 != nil {
				log.Errf("Failed to close file %s: %v", url, err1)
			}
		}()

		input = file
	}

	output, err := os.Create(target)
	if err != nil {
		return errors.Wrap(err, "Failed to create image file")
	}
	_, err = io.Copy(output, input)
	if err1 := output.Close(); err == nil {
		err = err1
	}
	log.Infof("Downloaded %v to %v", url, target)

	return err
}

func (s *DeploySrv) sanitizeApplication(app *pb.Application) error {
	c := s.cfg

	if app.Cores <= 0 {
		return fmt.Errorf("Cores value incorrect: %v", app.Cores)
	} else if app.Cores > c.MaxCores {
		return fmt.Errorf("Cores value over limit: %v > %v",
			app.Cores, c.MaxCores)
	}

	if app.Memory <= 0 {
		return fmt.Errorf("Memory value incorrect: %v", app.Memory)
	} else if app.Memory > c.MaxAppMem {
		return fmt.Errorf("Memory value over limit: %v > %v",
			app.Memory, c.MaxAppMem)
	}

	return nil
}

func (s *DeploySrv) deployCommon(ctx context.Context,
	dapp *metadata.DeployedApp) error {

	if err := s.sanitizeApplication(dapp.App); err != nil {
		return err
	}
	app2, err := s.meta.Load(dapp.App.Id)
	if err == nil && app2.IsDeployed {
		return status.Errorf(codes.AlreadyExists, "app %s already deployed",
			dapp.App.Id)
	}
	dapp.App.Status = pb.LifecycleStatus_DEPLOYING

	// TODO: either fix unmarshall of dapp.App.Source
	// or store the url directly in the DeployedApp structure
	source := dapp.App.Source
	dapp.App.Source = nil // reset source as can't unmarshall this

	// Initial save - creates the app directory if needed
	if err = dapp.Save(false); err != nil {
		return errors.Wrap(err, "metadata save failed")
	}

	/* Now download the image. */
	switch uri := source.(type) {
	case *pb.Application_HttpUri:
		dapp.URL = uri.HttpUri.HttpUri
		return downloadImage(dapp.URL, dapp.ImageFilePath(),
			s.cfg.DownloadTimeout.Duration)
	default:
		return status.Errorf(codes.Unimplemented, "unknown app source")
	}
}

// This function uses named return variables
func parseImageName(body io.Reader) (out string, hadTag bool, err error) {
	parsed := struct {
		Stream string
	}{}

	bytes, err := ioutil.ReadAll(body)
	if err != nil {
		return "", false, errors.Wrap(err,
			"failed to read JSON from docker.ImageLoad()")
	}
	err = json.Unmarshal(bytes, &parsed)

	// Validate output
	if err != nil {
		return "", false, errors.Wrap(err,
			"failed to parse docker image name")
	}
	if parsed.Stream == "" {
		return "", false, fmt.Errorf(
			"failed to parse docker image name: stream empty")
	}
	if !strings.Contains(parsed.Stream, "Loaded image") {
		return "", false, fmt.Errorf(
			"failed to parse docker image name: stream malformed")
	}

	out = strings.Replace(parsed.Stream, "Loaded image ID: ", "", 1)
	if strings.Contains(out, "Loaded image: ") {
		hadTag = true // Image already tagged, we'll need to untag
		out = strings.Replace(out, "Loaded image: ", "", 1)
	}
	out = out[0 : len(out)-1] // cut '\n'

	return out, hadTag, nil
}

func loadImage(ctx context.Context,
	dapp *metadata.DeployedApp, docker *client.Client) error {

	/* NOTE: ImageLoad could read directly from our HTTP stream that's
	 * downloading the image, thus removing the need for storing the image as
	 * a file. But store for now for easier debugging. */
	file, err := os.Open(dapp.ImageFilePath())
	if err != nil { /* shouldn't happen as we just wrote it */
		return errors.Wrap(err, "Failed to open image file")
	}

	respLoad, err := docker.ImageLoad(ctx, file, true)
	if err != nil {
		return errors.Wrap(err, "Failed to ImageLoad() the docker image")
	}
	defer func() {
		if err1 := respLoad.Body.Close(); err1 != nil {
			log.Errf("Failed to close docker reader %v", err1)
		}
	}()

	if !respLoad.JSON {
		return fmt.Errorf("No JSON output loading app %s", dapp.App.Id)
	}
	imageName, hadTag, err := parseImageName(respLoad.Body)
	if err != nil {
		return err
	}
	log.Infof("Image '%v' retagged to '%v'", imageName, dapp.App.Id)
	if err = docker.ImageTag(ctx, imageName, dapp.App.Id); err != nil {
		return err
	}
	if hadTag {
		_, err = docker.ImageRemove(ctx, imageName, types.ImageRemoveOptions{})
	}

	return err
}

// DeployContainer deploys a container
func (s *DeploySrv) DeployContainer(ctx context.Context,
	pbapp *pb.Application) (*empty.Empty, error) {

	dapp := s.meta.NewDeployedApp(metadata.Container, pbapp)
	if err := s.deployCommon(ctx, dapp); err != nil {
		return nil, errors.Wrap(err, "deployCommon() failed")
	}

	/* Now call the docker API. */
	docker, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create a docker client")
	}

	// Load the image first
	if err = loadImage(ctx, dapp, docker); err != nil {
		return nil, err
	}

	defer func() { /* We're far enough to warrant metadata update. */
		if err = dapp.Save(true); err != nil {
			log.Errf("Failed to save initial state of %v: %+v", pbapp.Id, err)
		}
	}()
	// Status will be error unless explicitly reset
	dapp.App.Status = pb.LifecycleStatus_ERROR

	if s.cfg.KubernetesMode { // this mode requires us to only upload the image
		if err = dapp.SetDeployed(""); err != nil {
			return nil, errors.Wrapf(err, "SetDeployed(%v) failed", pbapp.Id)
		}
		dapp.App.Status = pb.LifecycleStatus_READY

		return &empty.Empty{}, nil // success
	}

	// Now create a container out of the image
	resources := container.Resources{
		Memory:    int64(pbapp.Memory) * 1024 * 1024,
		CPUShares: int64(pbapp.Cores),
	}
	respCreate, err := docker.ContainerCreate(ctx,
		&container.Config{Image: pbapp.Id},
		&container.HostConfig{
			Resources: resources,
			CapAdd:    []string{"NET_ADMIN"}},
		nil, pbapp.Id)
	if err != nil {
		return nil, errors.Wrap(err, "ContinerCreate failed")
	}
	log.Infof("Created a container with id %v", respCreate.ID)

	// Deployment succeeded, update our metadata
	if err = dapp.SetDeployed(respCreate.ID); err != nil {
		return nil, errors.Wrapf(err, "SetDeployed(%v) failed", pbapp.Id)
	}
	dapp.App.Status = pb.LifecycleStatus_READY

	return &empty.Empty{}, nil
}

// DeployVM deploys VM
func (s *DeploySrv) DeployVM(ctx context.Context,
	pbapp *pb.Application) (*empty.Empty, error) {

	dapp := s.meta.NewDeployedApp(metadata.VM, pbapp)
	if err := s.deployCommon(ctx, dapp); err != nil {
		return nil, errors.Wrap(err, "deployCommon() failed")
	}

	/* Now call the libvirt API. */
	conn, err := libvirt.NewConnect("qemu:///system")
	if err != nil {
		return nil, err
	}
	defer func() {
		if c, err1 := conn.Close(); err1 != nil || c < 0 {
			log.Errf("Failed to close libvirt connection: code: %v, error: %v",
				c, err1)
		}
	}()

	// Round up to next 2 MiB boundary
	memRounded := math.Ceil(float64(pbapp.Memory)/2) * 2
	domcfg := libvirtxml.Domain{
		Type: "kvm", Name: pbapp.Id,
		OS: &libvirtxml.DomainOS{
			Type: &libvirtxml.DomainOSType{Arch: "x86_64", Type: "hvm"},
		},

		CPU: &libvirtxml.DomainCPU{
			Mode: "host-passthrough",
			Numa: &libvirtxml.DomainNuma{
				Cell: []libvirtxml.DomainCell{
					{
						ID:        new(uint), // it's initialized to 0
						CPUs:      fmt.Sprintf("0-%v", pbapp.Cores-1),
						Memory:    fmt.Sprintf("%v", memRounded),
						Unit:      "MiB",
						MemAccess: "shared",
					},
				},
			},
		},
		VCPU: &libvirtxml.DomainVCPU{Value: int(pbapp.Cores)},

		MemoryBacking: &libvirtxml.DomainMemoryBacking{
			MemoryHugePages: &libvirtxml.DomainMemoryHugepages{
				Hugepages: []libvirtxml.DomainMemoryHugepage{
					{Size: 2, Unit: "MiB"},
				},
			},
		},
		Devices: &libvirtxml.DomainDeviceList{
			Emulator: "/usr/local/bin/qemu-system-x86_64",
			Disks: []libvirtxml.DomainDisk{
				{
					Device: "disk",
					Driver: &libvirtxml.DomainDiskDriver{
						Name: "qemu",
						Type: "qcow2",
					},
					Source: &libvirtxml.DomainDiskSource{
						File: &libvirtxml.DomainDiskSourceFile{
							File: dapp.ImageFilePath()},
					},
					Target: &libvirtxml.DomainDiskTarget{Dev: "hda"},
				},
			},
			Interfaces: []libvirtxml.DomainInterface{
				{
					Source: &libvirtxml.DomainInterfaceSource{
						Network: &libvirtxml.DomainInterfaceSourceNetwork{
							Network: "default",
						},
					},
					Model: &libvirtxml.DomainInterfaceModel{Type: "virtio"},
				},
				{
					Source: &libvirtxml.DomainInterfaceSource{
						VHostUser: &libvirtxml.DomainChardevSource{
							UNIX: &libvirtxml.DomainChardevSourceUNIX{
								Path: s.cfg.VhostSocket, Mode: "client",
							},
						},
					},
					Model: &libvirtxml.DomainInterfaceModel{Type: "virtio"},
				},
			},
		},
	}

	xmldoc, err := domcfg.Marshal()
	if err != nil {
		return nil, err
	}
	log.Debugf("XML doc for %v:\n%v", pbapp.Id, xmldoc)

	dom, err := conn.DomainDefineXML(xmldoc)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dom.Free() }()
	name, err := dom.GetName()
	if err == nil {
		log.Infof("VM '%v' created", name)
	} else {
		log.Errf("Failed to get VM name of '%v'", pbapp.Id)
	}

	if err = dapp.SetDeployed(pbapp.Id); err != nil {
		return nil, err
	}
	dapp.App.Status = pb.LifecycleStatus_READY
	if err = dapp.Save(true); err != nil {
		return nil, err
	}

	return &empty.Empty{}, nil
}

// Redeploy trigger the deployment again
func (s *DeploySrv) Redeploy(ctx context.Context,
	app *pb.Application) (*empty.Empty, error) {

	dapp, err := s.meta.Load(app.Id)
	if err != nil {
		return nil, errors.Wrapf(err, "Application %v not found", app.Id)
	}

	if _, err = s.Undeploy(ctx, &pb.ApplicationID{Id: app.Id}); err != nil {
		return nil, errors.Wrapf(err, "Could not undeploy %v", app.Id)
	}

	switch dapp.Type {
	case metadata.Container:
		_, err = s.DeployContainer(ctx, app)
	case metadata.VM:
		_, err = s.DeployVM(ctx, app)
	default:
		err = status.Errorf(codes.Unimplemented, "not implemented app type")
	}

	return &empty.Empty{}, err
}

func (s *DeploySrv) dockerUndeploy(ctx context.Context,
	dapp *metadata.DeployedApp) error {

	docker, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return errors.Wrap(err, "Failed to create a docker client")
	}

	if dapp.DeployedID != "" {
		if dapp.App.GetStatus() == pb.LifecycleStatus_RUNNING {
			log.Warningf("Removing running container '%v'", dapp.DeployedID)
		}
		err = docker.ContainerRemove(ctx, dapp.DeployedID,
			types.ContainerRemoveOptions{Force: true})

		if err != nil {
			return errors.Wrapf(err, "Undeploy(%s)", dapp.DeployedID)
		}
		log.Infof("Removed container '%v'", dapp.DeployedID)
	} else if !s.cfg.KubernetesMode {
		log.Errf("Could not find container ID for '%v'", dapp.App.Id)
	}
	_, err = docker.ImageRemove(ctx, dapp.App.Id, types.ImageRemoveOptions{})
	if err != nil {
		return errors.Wrapf(err, "ImageRemove(%v) failed", dapp.App.Id)
	}
	log.Infof("Docker image '%v' removed", dapp.App.Id)

	return dapp.SetUndeployed()
}

func libvirtUndeploy(ctx context.Context, dapp *metadata.DeployedApp) error {

	conn, err := libvirt.NewConnect("qemu:///system")
	if err != nil {
		return err
	}
	defer func() {
		if c, err1 := conn.Close(); err1 != nil || c < 0 {
			log.Errf("Failed to close libvirt connection: code: %v, error: %v",
				c, err1)
		}
	}()

	dom, err := conn.LookupDomainByName(dapp.App.Id)
	if err != nil {
		return err
	}
	defer func() { _ = dom.Free() }()

	state, _, err := dom.GetState()
	if err != nil {
		log.Errf("Could not get domain '%v' state: %v", dapp.App.Id, err)
	}

	if state == libvirt.DOMAIN_RUNNING {
		log.Infof("Domain (VM) '%v' is running - stopping before undeploy",
			dapp.App.Id)
		if err = dom.Destroy(); err != nil {
			return errors.Wrapf(err, "Failed to destroy '%v'", dapp.App.Id)
		}
	}

	if err = dom.Undefine(); err != nil {
		return errors.Wrapf(err, "Failed to undefine '%v'", dapp.App.Id)
	}
	log.Infof("Domain (VM) '%v' undefined", dapp.App.Id)

	return dapp.SetUndeployed()
}

// Undeploy do the removel of deployment
func (s *DeploySrv) Undeploy(ctx context.Context,
	app *pb.ApplicationID) (*empty.Empty, error) {

	log.Infof("Undeploy(%s) running", app.Id)
	dapp, err := s.meta.Load(app.Id)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"Application %v not found: %v", app.Id, err)
	}
	if !dapp.IsDeployed {
		return nil, status.Errorf(codes.FailedPrecondition,
			"Application %v is not deployed", app.Id)
	}

	switch dapp.Type {
	case metadata.Container:
		err = s.dockerUndeploy(ctx, dapp)
	case metadata.VM:
		err = libvirtUndeploy(ctx, dapp)
	default:
		return nil, status.Errorf(codes.Unimplemented,
			"not implemented app type %v", dapp.Type)
	}

	defer func() {
		if err = dapp.Save(true); err != nil {
			log.Errf("Failed to save final state of %v: %+v", app.Id, err)
		}
	}()

	if err != nil {
		log.Errf("Undeploy(%v) failed: %+v", app.Id, err)
		dapp.App.Status = pb.LifecycleStatus_ERROR /* We're in a bad state.*/

		return nil, status.Errorf(codes.Internal,
			"Undeploy(%v) failed: %v", app.Id, err)
	}

	if os.Remove(dapp.ImageFilePath()) == nil {
		log.Infof("Deleted image file of %v", app.Id)
	}
	/* App is removed, no state left. */
	dapp.App.Status = pb.LifecycleStatus_UNKNOWN

	return &empty.Empty{}, nil
}
