//go:build openstack || !onlyprovider

package openstack

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/bootfromvolume"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/startstop"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/pagination"
	"github.com/nanovms/ops/lepton"
	"github.com/nanovms/ops/log"
	"github.com/olekukonko/tablewriter"
)

func getOpenStackInstances(provider *gophercloud.ProviderClient, opts servers.ListOpts) ([]lepton.CloudInstance, error) {
        log.Debug("Openstack getOpenStackInstances ...")
	cinstances := []lepton.CloudInstance{}

	client, err := openstack.NewComputeV2(provider, gophercloud.EndpointOpts{
		Region: os.Getenv("OS_REGION_NAME"),
	})
	if err != nil {
		log.Error(err)
	}

	pager := servers.List(client, opts)

	err = pager.EachPage(func(page pagination.Page) (bool, error) {
		serverList, err := servers.ExtractServers(page)
		if err != nil {
			log.Error(err)
			return false, err
		}

		for _, s := range serverList {
			if val, ok := s.Metadata["CreatedBy"]; ok && val == "ops" {
				ipv4 := ""

				// addresses may have a different structure in each cloud provider
				// vexx
				z := s.Addresses["public"]
				if z == nil {
					// ovh
					z = s.Addresses["Ext-Net"]
				}

				if z != nil {
					for _, v := range z.([]interface{}) {
						sz := v.(map[string]interface{})
						version := sz["version"].(float64)
						if version == 4 {
							ipv4 = sz["addr"].(string)
						}
					}
				}

				cinstance := lepton.CloudInstance{
					ID:      s.ID,
					Name:    s.Name,
					Status:  s.Status,
					Created: s.Created.Format("2006-01-02 15:04:05"),
				}

				if val, ok := s.Metadata["Image"]; ok {
					cinstance.Image = val
				}

				if ipv4 != "" {
					cinstance.PublicIps = []string{ipv4}
				}

				cinstances = append(cinstances, cinstance)
			}
		}

		return true, nil
	})

        log.Debug(fmt.Sprintf("cinstances=%s", cinstances))
	return cinstances, nil
}

// CreateInstance - Creates instance on OpenStack.
func (o *OpenStack) CreateInstance(ctx *lepton.Context) error {
        log.Debug("Openstack CreateInstance ...")
	client, err := o.getComputeClient()
	if err != nil {
		log.Error(err)
	}

	imageName := ctx.Config().CloudConfig.ImageName
	log.Info(fmt.Sprintf("Looking up image: %s", imageName))

	imageID, err := o.findImage(imageName)
	if err != nil {
		log.Error(err)
		return err
	}

	log.Info(fmt.Sprintf("Deploying imageID: %s", imageID))

	flavorID, err := o.findFlavorByName(ctx.Config().CloudConfig.Flavor)

	if err != nil {
		log.Error(err)
	}

	log.Info(fmt.Sprintf("Deploying flavorID: %s", flavorID))

	var networks interface{}
	targetConfig := ctx.Config().TargetConfig
	log.Debug(fmt.Sprintf("DEBUG TargetConfig: %s", targetConfig))
	networkUUID := targetConfig["NetworkUUID"]
	log.Info(fmt.Sprintf("Deploying NetworkUUID: %s", networkUUID))
	fixedIP := targetConfig["FixedIP"]
	log.Info(fmt.Sprintf("Deploying FixedIP: %s", fixedIP))
	if networkUUID != "" {
		log.Debug(fmt.Sprintf("DEBUG NetworkUUID is sets"))
		networks = []servers.Network{{UUID: networkUUID, FixedIP: fixedIP}}
	} else {
		networks = "auto"
	}
	log.Debug(fmt.Sprintf("DEBUG networks=%+v", networks))

	instanceName := ctx.Config().RunConfig.InstanceName

	var createOpts servers.CreateOptsBuilder
	createOpts = &servers.CreateOpts{
		Name:      instanceName,
		ImageRef:  imageID,
		FlavorRef: flavorID,
		AdminPass: "TODO",
		Metadata:  map[string]string{"CreatedBy": "ops", "Image": imageName},
		Networks:  networks,
	}
	log.Debug(fmt.Sprintf("DEBUG: createOpts1=%+v", createOpts))

	var volumeSize int
	if ctx.Config().RunConfig.VolumeSizeInGb == 0 {
		volumeSize = 1
	} else {
		volumeSize = ctx.Config().RunConfig.VolumeSizeInGb
	}

	var destinationType bootfromvolume.DestinationType
	destinationTypeRequested := targetConfig["DestinationType"]
	log.Debug(fmt.Sprintf("destinationTypeRequested: %s", destinationTypeRequested))
	if destinationTypeRequested != "" {
		lowerDestinationTypeRequested := strings.ToLower(destinationTypeRequested)
		if lowerDestinationTypeRequested == "volume" {
			destinationType = bootfromvolume.DestinationVolume
		} else if lowerDestinationTypeRequested == "local" {
			destinationType = bootfromvolume.DestinationLocal
		} else {
			return errors.New("Unsupported DestinationType specified in TargetConfig")
		}
	} else {
		destinationType = bootfromvolume.DestinationVolume
	}
	log.Info(fmt.Sprintf("Deploying to DestinationType: %s", destinationType))

	createOpts = o.addBootFromVolumeParams(createOpts, imageID, volumeSize, destinationType)
	log.Debug(fmt.Sprintf("DEBUG: createOpts2=%+v", createOpts))
	server, err := servers.Create(client, createOpts).Extract()

	if err != nil {
		return errors.New(err.Error())
	}

	log.Info(fmt.Sprintf("Instance Created Successfully. ID ---> %s | Name ---> %s\n", server.ID, instanceName))

	if ctx.Config().CloudConfig.DomainName != "" {
		pollCount := 60
		for pollCount > 0 {
			fmt.Printf(".")
			time.Sleep(2 * time.Second)

			instance, err := o.GetInstanceByName(ctx, server.Name)
			if err != nil || len(instance.PublicIps) == 0 {
				pollCount--
				continue
			}

			if len(instance.PublicIps) != 0 {
				err := lepton.CreateDNSRecord(ctx.Config(), instance.PublicIps[0], o)
				if err != nil {
					return err
				}
			}
			return nil
		}
	}

	return nil
}

func (o *OpenStack) addBootFromVolumeParams(
	createOpts servers.CreateOptsBuilder,
	imageID string,
	rootDiskSizeGb int,
	destinationType bootfromvolume.DestinationType,
) *bootfromvolume.CreateOptsExt {
	log.Debug(fmt.Sprintf("DEBUG: createOpts3=%+v\n", createOpts))
        
	blockDevice := bootfromvolume.BlockDevice{
		BootIndex:           0,
		DeleteOnTermination: true,
		DestinationType:     destinationType,
		SourceType:          bootfromvolume.SourceType("image"),
		UUID:                imageID,
	}
	log.Debug(fmt.Sprintf("DEBUG: blockDevice=%+v\n", blockDevice))
	if rootDiskSizeGb > 0 {
		blockDevice.VolumeSize = rootDiskSizeGb
	}

	return &bootfromvolume.CreateOptsExt{
		CreateOptsBuilder: createOpts,
		BlockDevice:       []bootfromvolume.BlockDevice{blockDevice},
	}
}

// GetInstanceByName returns instance with given name
func (o *OpenStack) GetInstanceByName(ctx *lepton.Context, name string) (*lepton.CloudInstance, error) {
        log.Debug("Openstack GetInstanceByName ...")
	opts := servers.ListOpts{
		Name: name,
	}

	instances, err := getOpenStackInstances(o.provider, opts)
	if err != nil {
		return nil, err
	}

	if len(instances) == 0 {
		return nil, lepton.ErrInstanceNotFound(name)
	}

	return &instances[0], nil
}

// GetInstances return all instances on OpenStack
func (o *OpenStack) GetInstances(ctx *lepton.Context) ([]lepton.CloudInstance, error) {
        log.Debug("Openstack GetInstances ...")
	return getOpenStackInstances(o.provider, servers.ListOpts{})
}

// ListInstances lists instances on OpenStack.
// It essentially does:
func (o *OpenStack) ListInstances(ctx *lepton.Context) error {
	cinstances, err := o.GetInstances(ctx)
	if err != nil {
		return err
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"ID", "Name", "IP", "Status", "Created", "Image"})
	table.SetHeaderColor(
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.Bold, tablewriter.FgCyanColor})
	table.SetRowLine(true)

	for _, instance := range cinstances {
		var row []string

		row = append(row, instance.ID)
		row = append(row, instance.Name)
		row = append(row, strings.Join(instance.PublicIps, ","))
		row = append(row, instance.Status)
		row = append(row, instance.Created)
		row = append(row, instance.Image)

		table.Append(row)
	}

	table.Render()

	return nil
}

// DeleteInstance deletes instance from OpenStack
func (o *OpenStack) DeleteInstance(ctx *lepton.Context, instancename string) error {
        log.Debug("Openstack DeleteInstance ...")

	instances, err := o.GetInstances(ctx)

	client, err := openstack.NewComputeV2(o.provider, gophercloud.EndpointOpts{
		Region: os.Getenv("OS_REGION_NAME"),
	})

	if err != nil {
		return err
	}

	if len(instances) == 0 {
		return errors.New("No Instance available for deletion")
	}

	for _, instance := range instances {
		if instance.Name == instancename {
			log.Debug("Instance Name match found.")
			result := servers.Delete(client, instance.ID).ExtractErr()

			if result == nil {
				log.Info(fmt.Sprintf("Deleted instance with ID %s and name %s\n", instance.ID, instancename))
			} else {
				return errors.New(result.Error())
			}

		}
	}

	return nil
}

// RebootInstance reboots the instance.
func (o *OpenStack) RebootInstance(ctx *lepton.Context, instancename string) error {
	return fmt.Errorf("operation not supported")
}

// StartInstance starts an instance in OpenStack.
func (o *OpenStack) StartInstance(ctx *lepton.Context, instancename string) error {
	client, err := o.getComputeClient()
	if err != nil {
		log.Error(err)
	}

	server, err := o.findInstance(instancename)
	if err != nil {
		log.Error(err)
	}

	err = startstop.Start(client, server.ID).ExtractErr()
	if err != nil {
		log.Error(err)
	}

	return nil
}

// StopInstance stops an instance from OpenStack
func (o *OpenStack) StopInstance(ctx *lepton.Context, instancename string) error {
	client, err := o.getComputeClient()
	if err != nil {
		log.Error(err)
	}

	server, err := o.findInstance(instancename)
	if err != nil {
		log.Error(err)
	}

	err = startstop.Stop(client, server.ID).ExtractErr()
	if err != nil {
		log.Error(err)
	}

	return nil
}

func (o *OpenStack) findInstance(name string) (volume *servers.Server, err error) {
	var server *servers.Server

	client, err := o.getComputeClient()
	if err != nil {
		log.Error(err)
	}

	opts := servers.ListOpts{}

	pager := servers.List(client, opts)

	err = pager.EachPage(func(page pagination.Page) (bool, error) {
		serverList, err := servers.ExtractServers(page)
		if err != nil {
			log.Error(err)
			return false, err
		}

		for _, s := range serverList {
			if s.Name == name {
				server = &s
				return true, nil
			}
		}

		return true, nil
	})

	if server != nil {
		return server, nil
	}

	return nil, errors.New("could not find server")
}

// PrintInstanceLogs writes instance logs to console
func (o *OpenStack) PrintInstanceLogs(ctx *lepton.Context, instancename string, watch bool) error {
	l, err := o.GetInstanceLogs(ctx, instancename)
	if err != nil {
		return err
	}
	fmt.Printf(l)
	return nil
}

// GetInstanceLogs gets instance related logs.
func (o *OpenStack) GetInstanceLogs(ctx *lepton.Context, instancename string) (string, error) {

	client, err := o.getComputeClient()
	if err != nil {
		return "", err
	}

	server, err := o.findInstance(instancename)
	if err != nil {
		return "", err
	}

	outputOpts := &servers.ShowConsoleOutputOpts{
		Length: 100,
	}
	output, err := servers.ShowConsoleOutput(client, server.ID, outputOpts).Extract()
	if err != nil {
		return "", err
	}

	return output, nil
}

// InstanceStats show metrics for instances on openstack
func (o *OpenStack) InstanceStats(ctx *lepton.Context, instancename string, watch bool) error {
	return errors.New("currently not avilable")
}
