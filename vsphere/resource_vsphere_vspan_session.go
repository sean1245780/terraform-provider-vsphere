package vsphere

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-provider-vsphere/vsphere/internal/helper/customattribute"
	"github.com/hashicorp/terraform-provider-vsphere/vsphere/internal/helper/folder"
	"github.com/hashicorp/terraform-provider-vsphere/vsphere/internal/helper/structure"
	"github.com/hashicorp/terraform-provider-vsphere/vsphere/internal/helper/viapi"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
)

func resourceVSphereDistributedVirtualSwitch() *schema.Resource {
	s := map[string]*schema.Schema{
		"datacenter_id": {
			Type:        schema.TypeString,
			Description: "The ID of the datacenter to create this virtual switch in.",
			Required:    true,
			ForceNew:    true,
		},
		"folder": {
			Type:        schema.TypeString,
			Description: "The folder to create this virtual switch in, relative to the datacenter.",
			Optional:    true,
			ForceNew:    true,
		},
		"network_resource_control_enabled": {
			Type:        schema.TypeBool,
			Description: "Whether or not to enable network resource control, enabling advanced traffic shaping and resource control features.",
			Optional:    true,
		},
		// Tagging
		vSphereTagAttributeKey:    tagsSchema(),
		customattribute.ConfigKey: customattribute.ConfigSchema(),
	}
	structure.MergeSchema(s, schemaDVSCreateSpec())

	return &schema.Resource{
		Create: resourceVSphereDistributedVirtualSwitchCreate,
		Read:   resourceVSphereDistributedVirtualSwitchRead,
		Update: resourceVSphereDistributedVirtualSwitchUpdate,
		Delete: resourceVSphereDistributedVirtualSwitchDelete,
		Importer: &schema.ResourceImporter{
			State: resourceVSphereDistributedVirtualSwitchImport,
		},
		Schema: s,
	}
}

func resourceVSphereDistributedVirtualSwitchCreate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*Client).vimClient
	if err := viapi.ValidateVirtualCenter(client); err != nil {
		return err
	}
	tagsClient, err := tagsManagerIfDefined(d, meta)
	if err != nil {
		return err
	}
	// Verify a proper vCenter before proceeding if custom attributes are defined
	attrsProcessor, err := customattribute.GetDiffProcessorIfAttributesDefined(client, d)
	if err != nil {
		return err
	}

	dc, err := datacenterFromID(client, d.Get("datacenter_id").(string))
	if err != nil {
		return fmt.Errorf("cannot locate datacenter: %s", err)
	}
	fo, err := folder.FromPath(client, d.Get("folder").(string), folder.VSphereFolderTypeNetwork, dc)
	if err != nil {
		return fmt.Errorf("cannot locate folder: %s", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultAPITimeout)
	defer cancel()
	spec := expandDVSCreateSpec(d)
	task, err := fo.CreateDVS(ctx, spec)
	if err != nil {
		return fmt.Errorf("error creating DVS: %s", err)
	}
	tctx, tcancel := context.WithTimeout(context.Background(), defaultAPITimeout)
	defer tcancel()
	info, err := task.WaitForResult(tctx, nil)
	if err != nil {
		return fmt.Errorf("error waiting for DVS creation to complete: %s", err)
	}

	dvs, err := dvsFromMOID(client, info.Result.(types.ManagedObjectReference).Value)
	if err != nil {
		return fmt.Errorf("error fetching DVS after creation: %s", err)
	}
	props, err := dvsProperties(dvs)
	if err != nil {
		return fmt.Errorf("error fetching DVS properties after creation: %s", err)
	}

	d.SetId(props.Uuid)

	// Enable network resource I/O control if it needs to be enabled
	if d.Get("network_resource_control_enabled").(bool) {
		err = enableDVSNetworkResourceManagement(client, dvs, true)
		if err != nil {
			return err
		}
	}

	// Apply any pending tags now
	if tagsClient != nil {
		if err := processTagDiff(tagsClient, d, object.NewReference(client.Client, dvs.Reference())); err != nil {
			return fmt.Errorf("error updating tags: %s", err)
		}
	}

	// Set custom attributes
	if attrsProcessor != nil {
		if err := attrsProcessor.ProcessDiff(dvs); err != nil {
			return err
		}
	}

	return resourceVSphereDistributedVirtualSwitchRead(d, meta)
}

func resourceVSphereDistributedVirtualSwitchRead(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*Client).vimClient
	if err := viapi.ValidateVirtualCenter(client); err != nil {
		return err
	}
	id := d.Id()
	dvs, err := dvsFromUUID(client, id)
	if err != nil {
		return fmt.Errorf("could not find DVS %q: %s", id, err)
	}
	props, err := dvsProperties(dvs)
	if err != nil {
		return fmt.Errorf("error fetching DVS properties: %s", err)
	}

	// Set the datacenter ID, for completion's sake when importing
	dcp, err := folder.RootPathParticleNetwork.SplitDatacenter(dvs.InventoryPath)
	if err != nil {
		return fmt.Errorf("error parsing datacenter from inventory path: %s", err)
	}
	dc, err := getDatacenter(client, dcp)
	if err != nil {
		return fmt.Errorf("error locating datacenter: %s", err)
	}
	_ = d.Set("datacenter_id", dc.Reference().Value)

	// Set the folder
	f, err := folder.RootPathParticleNetwork.SplitRelativeFolder(dvs.InventoryPath)
	if err != nil {
		return fmt.Errorf("error parsing DVS path %q: %s", dvs.InventoryPath, err)
	}
	_ = d.Set("folder", folder.NormalizePath(f))

	// Read in config info
	if err := flattenVMwareDVSConfigInfo(d, props.Config.(*types.VMwareDVSConfigInfo)); err != nil {
		return err
	}

	// Read tags if we have the ability to do so
	if tagsClient, _ := meta.(*Client).TagsManager(); tagsClient != nil {
		if err := readTagsForResource(tagsClient, dvs, d); err != nil {
			return fmt.Errorf("error reading tags: %s", err)
		}
	}

	// Read set custom attributes
	if customattribute.IsSupported(client) {
		customattribute.ReadFromResource(props.Entity(), d)
	}

	return nil
}

func resourceVSphereDistributedVirtualSwitchUpdate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*Client).vimClient
	if err := viapi.ValidateVirtualCenter(client); err != nil {
		return err
	}
	tagsClient, err := tagsManagerIfDefined(d, meta)
	if err != nil {
		return err
	}
	// Verify a proper vCenter before proceeding if custom attributes are defined
	attrsProcessor, err := customattribute.GetDiffProcessorIfAttributesDefined(client, d)
	if err != nil {
		return err
	}

	id := d.Id()
	dvs, err := dvsFromUUID(client, id)
	if err != nil {
		return fmt.Errorf("could not find DVS %q: %s", id, err)
	}

	// If we have a pending version upgrade, do that first.
	if d.HasChange("version") {
		old, newValue := d.GetChange("version")
		var ovi, nvi int
		for n, v := range dvsVersions {
			if old.(string) == v {
				ovi = n
			}
			if newValue.(string) == v {
				nvi = n
			}
		}
		if nvi < ovi {
			return fmt.Errorf("downgrading dvSwitches are not allowed (old: %s new: %s)", old, newValue)
		}
		if err := upgradeDVS(client, dvs, newValue.(string)); err != nil {
			return fmt.Errorf("could not upgrade DVS: %s", err)
		}
		props, err := dvsProperties(dvs)
		if err != nil {
			return fmt.Errorf("could not get DVS properties after upgrade: %s", err)
		}
		// ConfigVersion increments after a DVS upgrade, which means this needs to
		// be updated before the post-update read to ensure that we don't run into
		// ConcurrentAccess errors on the update operation below.
		_ = d.Set("config_version", props.Config.(*types.VMwareDVSConfigInfo).ConfigVersion)
	}

	spec := expandVMwareDVSConfigSpec(d)
	if err := updateDVSConfiguration(dvs, spec); err != nil {
		return fmt.Errorf("could not update DVS: %s", err)
	}

	// Modify network I/O control if necessary
	if d.HasChange("network_resource_control_enabled") {
		err = enableDVSNetworkResourceManagement(client, dvs, d.Get("network_resource_control_enabled").(bool))
		if err != nil {
			return err
		}
	}

	// Apply any pending tags now
	if tagsClient != nil {
		if err := processTagDiff(tagsClient, d, object.NewReference(client.Client, dvs.Reference())); err != nil {
			return fmt.Errorf("error updating tags: %s", err)
		}
	}

	// Apply custom attribute updates
	if attrsProcessor != nil {
		if err := attrsProcessor.ProcessDiff(dvs); err != nil {
			return err
		}
	}

	return resourceVSphereDistributedVirtualSwitchRead(d, meta)
}

func resourceVSphereDistributedVirtualSwitchDelete(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*Client).vimClient
	if err := viapi.ValidateVirtualCenter(client); err != nil {
		return err
	}
	id := d.Id()
	dvs, err := dvsFromUUID(client, id)
	if err != nil {
		return fmt.Errorf("could not find DVS %q: %s", id, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultAPITimeout)
	defer cancel()
	task, err := dvs.Destroy(ctx)
	if err != nil {
		return fmt.Errorf("error deleting DVS: %s", err)
	}
	tctx, tcancel := context.WithTimeout(context.Background(), defaultAPITimeout)
	defer tcancel()
	if err := task.Wait(tctx); err != nil {
		return fmt.Errorf("error waiting for DVS deletion to complete: %s", err)
	}

	return nil
}

func resourceVSphereDistributedVirtualSwitchImport(d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
	// Due to the relative difficulty in trying to fetch a DVS's UUID, we use the
	// inventory path to the DVS instead, and just run it through finder. A full
	// path is required unless the default datacenter can be utilized.
	client := meta.(*Client).vimClient
	if err := viapi.ValidateVirtualCenter(client); err != nil {
		return nil, err
	}
	p := d.Id()
	dvs, err := dvsFromPath(client, p, nil)
	if err != nil {
		return nil, fmt.Errorf("error locating DVS: %s", err)
	}
	props, err := dvsProperties(dvs)
	if err != nil {
		return nil, fmt.Errorf("error fetching DVS properties: %s", err)
	}
	d.SetId(props.Uuid)
	return []*schema.ResourceData{d}, nil
}
/*


YAAAAAPPPPP



*/
package vsphere

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-provider-vsphere/vsphere/internal/helper/clustercomputeresource"
	"github.com/hashicorp/terraform-provider-vsphere/vsphere/internal/helper/structure"
	"github.com/hashicorp/terraform-provider-vsphere/vsphere/internal/helper/viapi"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
)

const resourceVSphereComputeClusterVMHostRuleName = "vsphere_compute_cluster_vm_host_rule"

func resourceVSphereVspanSession() *schema.Resource {
	return &schema.Resource{
		Create:        resourceVSphereComputeClusterVMHostRuleCreate,
		Read:          resourceVSphereComputeClusterVMHostRuleRead,
		Update:        resourceVSphereComputeClusterVMHostRuleUpdate,
		Delete:        resourceVSphereComputeClusterVMHostRuleDelete,
		CustomizeDiff: resourceVSphereComputeClusterVMHostRuleCustomizeDiff,
		Importer: &schema.ResourceImporter{
			State: resourceVSphereComputeClusterVMHostRuleImport,
		},

		Schema: map[string]*schema.Schema{
			"name": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "The display name.",
			},
			"description": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "The description for the session.",
			},
			"key": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "The generated key as the identifier for the session.",
			},
			"mirrored_packet_length": {
				Type:          schema.TypeInt,
				Optional:      true,
				Description:   "An integer that describes how much of each frame to mirror. If unset, all of the frame would be mirrored. Setting this property to a smaller value is useful when the consumer will look only at the headers. The value cannot be less than 60.",
				ValidateFunc:  validation.IntBetween(60, 9000),
			},
			"encapsulation_vlan_id": {
				Type:          schema.TypeInt,
				Optional:      true,
				Description:   "VLAN ID used to encapsulate the mirrored traffic.",
				ValidateFunc: validation.IntBetween(1, 4094),
			},
			"enabled": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     true,
				Description: "Enable this rule in the cluster.",
			},
			"mandatory": {
				Type:        schema.TypeBool,
				Optional:    true,
				Description: "When true, prevents any virtual machine operations that may violate this rule.",
			},
		},
	}
}

func resourceVSphereComputeClusterVMHostRuleCreate(d *schema.ResourceData, meta interface{}) error {
	log.Printf("[DEBUG] %s: Beginning create", resourceVSphereComputeClusterVMHostRuleIDString(d))

	cluster, _, err := resourceVSphereComputeClusterVMHostRuleObjects(d, meta)
	if err != nil {
		return err
	}

	info, err := expandClusterVMHostRuleInfo(d)
	if err != nil {
		return err
	}
	spec := &types.ClusterConfigSpecEx{
		RulesSpec: []types.ClusterRuleSpec{
			{
				ArrayUpdateSpec: types.ArrayUpdateSpec{
					Operation: types.ArrayUpdateOperationAdd,
				},
				Info: info,
			},
		},
	}

	if err = clustercomputeresource.Reconfigure(cluster, spec); err != nil {
		return err
	}

	info, err = resourceVSphereComputeClusterVMHostRuleFindEntryByName(cluster, info.Name)
	if err != nil {
		return err
	}

	id, err := resourceVSphereComputeClusterVMHostRuleFlattenID(cluster, info.Key)
	if err != nil {
		return fmt.Errorf("cannot compute ID of created resource: %s", err)
	}
	d.SetId(id)

	log.Printf("[DEBUG] %s: Create finished successfully", resourceVSphereComputeClusterVMHostRuleIDString(d))
	return resourceVSphereComputeClusterVMHostRuleRead(d, meta)
}

func resourceVSphereComputeClusterVMHostRuleRead(d *schema.ResourceData, meta interface{}) error {
	log.Printf("[DEBUG] %s: Beginning read", resourceVSphereComputeClusterVMHostRuleIDString(d))

	cluster, key, err := resourceVSphereComputeClusterVMHostRuleObjects(d, meta)
	if err != nil {
		return err
	}

	info, err := resourceVSphereComputeClusterVMHostRuleFindEntry(cluster, key)
	if err != nil {
		return err
	}

	if info == nil {
		// The configuration is missing, blank out the ID so it can be re-created.
		d.SetId("")
		return nil
	}

	// Save the compute_cluster_id. This is ForceNew, but we set these for
	// completeness on import so that if the wrong cluster/VM combo was used, it
	// will be noted.
	if err = d.Set("compute_cluster_id", cluster.Reference().Value); err != nil {
		return fmt.Errorf("error setting attribute \"compute_cluster_id\": %s", err)
	}

	if err = flattenClusterVMHostRuleInfo(d, info); err != nil {
		return err
	}

	log.Printf("[DEBUG] %s: Read completed successfully", resourceVSphereComputeClusterVMHostRuleIDString(d))
	return nil
}

func resourceVSphereComputeClusterVMHostRuleUpdate(d *schema.ResourceData, meta interface{}) error {
	log.Printf("[DEBUG] %s: Beginning update", resourceVSphereComputeClusterVMHostRuleIDString(d))

	cluster, key, err := resourceVSphereComputeClusterVMHostRuleObjects(d, meta)
	if err != nil {
		return err
	}

	info, err := expandClusterVMHostRuleInfo(d)
	if err != nil {
		return err
	}
	info.Key = key

	spec := &types.ClusterConfigSpecEx{
		RulesSpec: []types.ClusterRuleSpec{
			{
				ArrayUpdateSpec: types.ArrayUpdateSpec{
					Operation: types.ArrayUpdateOperationEdit,
				},
				Info: info,
			},
		},
	}

	if err := clustercomputeresource.Reconfigure(cluster, spec); err != nil {
		return err
	}

	log.Printf("[DEBUG] %s: Update finished successfully", resourceVSphereComputeClusterVMHostRuleIDString(d))
	return resourceVSphereComputeClusterVMHostRuleRead(d, meta)
}

func resourceVSphereComputeClusterVMHostRuleDelete(d *schema.ResourceData, meta interface{}) error {
	log.Printf("[DEBUG] %s: Beginning delete", resourceVSphereComputeClusterVMHostRuleIDString(d))

	cluster, key, err := resourceVSphereComputeClusterVMHostRuleObjects(d, meta)
	if err != nil {
		return err
	}

	spec := &types.ClusterConfigSpecEx{
		RulesSpec: []types.ClusterRuleSpec{
			{
				ArrayUpdateSpec: types.ArrayUpdateSpec{
					Operation: types.ArrayUpdateOperationRemove,
					RemoveKey: key,
				},
			},
		},
	}

	if err := clustercomputeresource.Reconfigure(cluster, spec); err != nil {
		return err
	}

	log.Printf("[DEBUG] %s: Deleted successfully", resourceVSphereComputeClusterVMHostRuleIDString(d))
	return nil
}

func resourceVSphereComputeClusterVMHostRuleCustomizeDiff(_ context.Context, d *schema.ResourceDiff, _ interface{}) error {
	log.Printf("[DEBUG] %s: Beginning diff customization and validation", resourceVSphereComputeClusterVMHostRuleIDString(d))

	if err := resourceVSphereComputeClusterVMHostRuleValidateHostRulesSpecified(d); err != nil {
		return err
	}

	log.Printf("[DEBUG] %s: Diff customization and validation complete", resourceVSphereComputeClusterVMHostRuleIDString(d))
	return nil
}

func resourceVSphereComputeClusterVMHostRuleValidateHostRulesSpecified(d *schema.ResourceDiff) error {
	log.Printf(
		"[DEBUG] %s: Validating presence of one of affinity_host_group_name or anti_affinity_host_group_name",
		resourceVSphereComputeClusterVMHostRuleIDString(d),
	)
	_, affinityOk := d.GetOk("affinity_host_group_name")
	affinityKnown := d.NewValueKnown("affinity_host_group_name")
	_, antiOk := d.GetOk("anti_affinity_host_group_name")
	antiKnown := d.NewValueKnown("anti_affinity_host_group_name")

	if !affinityOk && affinityKnown && !antiOk && antiKnown {
		return errors.New("one of affinity_host_group_name or anti_affinity_host_group_name must be specified")
	}

	return nil
}

func resourceVSphereComputeClusterVMHostRuleImport(d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
	var data map[string]string
	if err := json.Unmarshal([]byte(d.Id()), &data); err != nil {
		return nil, err
	}
	clusterPath, ok := data["compute_cluster_path"]
	if !ok {
		return nil, errors.New("missing compute_cluster_path in input data")
	}
	name, ok := data["name"]
	if !ok {
		return nil, errors.New("missing name in input data")
	}

	client, err := resourceVSphereComputeClusterVMHostRuleClient(meta)
	if err != nil {
		return nil, err
	}

	cluster, err := clustercomputeresource.FromPath(client, clusterPath, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot locate cluster %q: %s", clusterPath, err)
	}

	info, err := resourceVSphereComputeClusterVMHostRuleFindEntryByName(cluster, name)
	if err != nil {
		return nil, err
	}

	id, err := resourceVSphereComputeClusterVMHostRuleFlattenID(cluster, info.Key)
	if err != nil {
		return nil, fmt.Errorf("cannot compute ID of imported resource: %s", err)
	}
	d.SetId(id)
	return []*schema.ResourceData{d}, nil
}

// expandClusterVMHostRuleInfo reads certain ResourceData keys and returns a
// ClusterVmHostRuleInfo.
func expandClusterVMHostRuleInfo(d *schema.ResourceData) (*types.ClusterVmHostRuleInfo, error) {
	obj := &types.ClusterVmHostRuleInfo{
		ClusterRuleInfo: types.ClusterRuleInfo{
			Enabled:     structure.GetBool(d, "enabled"),
			Mandatory:   structure.GetBool(d, "mandatory"),
			Name:        d.Get("name").(string),
			UserCreated: structure.BoolPtr(true),
		},
		AffineHostGroupName:     d.Get("affinity_host_group_name").(string),
		AntiAffineHostGroupName: d.Get("anti_affinity_host_group_name").(string),
		VmGroupName:             d.Get("vm_group_name").(string),
	}
	return obj, nil
}

// flattenClusterVMHostRuleInfo saves a ClusterVmHostRuleInfo into the supplied ResourceData.
func flattenClusterVMHostRuleInfo(d *schema.ResourceData, obj *types.ClusterVmHostRuleInfo) error {
	return structure.SetBatch(d, map[string]interface{}{
		"enabled":                       obj.Enabled,
		"mandatory":                     obj.Mandatory,
		"name":                          obj.Name,
		"affinity_host_group_name":      obj.AffineHostGroupName,
		"anti_affinity_host_group_name": obj.AntiAffineHostGroupName,
		"vm_group_name":                 obj.VmGroupName,
	})
}

// resourceVSphereComputeClusterVMHostRuleIDString prints a friendly string for the
// vsphere_compute_cluster_vm_host_rule resource.
func resourceVSphereComputeClusterVMHostRuleIDString(d structure.ResourceIDStringer) string {
	return structure.ResourceIDString(d, resourceVSphereComputeClusterVMHostRuleName)
}

// resourceVSphereComputeClusterVMHostRuleFlattenID makes an ID for the
// vsphere_compute_cluster_vm_host_rule resource.
func resourceVSphereComputeClusterVMHostRuleFlattenID(cluster *object.ClusterComputeResource, key int32) (string, error) {
	clusterID := cluster.Reference().Value
	return strings.Join([]string{clusterID, strconv.Itoa(int(key))}, ":"), nil
}

// resourceVSphereComputeClusterVMHostRuleParseID parses an ID for the
// vsphere_compute_cluster_vm_host_rule and outputs its parts.
func resourceVSphereComputeClusterVMHostRuleParseID(id string) (string, int32, error) {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) < 2 {
		return "", 0, fmt.Errorf("bad ID %q", id)
	}

	key, err := strconv.ParseInt(parts[1], 10, 32)
	if err != nil {
		return "", 0, fmt.Errorf("while converting key in ID %q to int32: %s", parts[1], err)
	}

	return parts[0], int32(key), nil
}

// resourceVSphereComputeClusterVMHostRuleFindEntry attempts to locate an
// existing VM/host rule in a cluster's configuration by key. It's used by the
// resource's read functionality and tests. nil is returned if the entry cannot
// be found.
func resourceVSphereComputeClusterVMHostRuleFindEntry(
	cluster *object.ClusterComputeResource,
	key int32,
) (*types.ClusterVmHostRuleInfo, error) {
	props, err := clustercomputeresource.Properties(cluster)
	if err != nil {
		return nil, fmt.Errorf("error fetching cluster properties: %s", err)
	}

	for _, info := range props.ConfigurationEx.(*types.ClusterConfigInfoEx).Rule {
		if info.GetClusterRuleInfo().Key == key {
			if vmHostRuleInfo, ok := info.(*types.ClusterVmHostRuleInfo); ok {
				log.Printf("[DEBUG] Found VM/host rule key %d in cluster %q", key, cluster.Name())
				return vmHostRuleInfo, nil
			}
			return nil, fmt.Errorf("rule key %d in cluster %q is not a VM/host rule", key, cluster.Name())
		}
	}

	log.Printf("[DEBUG] No VM/host rule key %d found in cluster %q", key, cluster.Name())
	return nil, nil
}

// resourceVSphereComputeClusterVMHostRuleFindEntryByName attempts to locate an
// existing VM/host rule in a cluster's configuration by name. It differs from
// the standard resourceVSphereComputeClusterVMHostRuleFindEntry in that we
// don't allow missing entries, as it's designed to be used in places where we
// don't want to allow for missing entries, such as during creation and import.
func resourceVSphereComputeClusterVMHostRuleFindEntryByName(
	cluster *object.ClusterComputeResource,
	name string,
) (*types.ClusterVmHostRuleInfo, error) {
	props, err := clustercomputeresource.Properties(cluster)
	if err != nil {
		return nil, fmt.Errorf("error fetching cluster properties: %s", err)
	}

	for _, info := range props.ConfigurationEx.(*types.ClusterConfigInfoEx).Rule {
		if info.GetClusterRuleInfo().Name == name {
			if vmHostRuleInfo, ok := info.(*types.ClusterVmHostRuleInfo); ok {
				log.Printf("[DEBUG] Found VM/host rule %q in cluster %q", name, cluster.Name())
				return vmHostRuleInfo, nil
			}
			return nil, fmt.Errorf("rule %q in cluster %q is not a VM/host rule", name, cluster.Name())
		}
	}

	return nil, fmt.Errorf("no VM/host rule %q found in cluster %q", name, cluster.Name())
}

// resourceVSphereComputeClusterVMHostRuleObjects handles the fetching of the
// cluster and rule key depending on what attributes are available:
// * If the resource ID is available, the data is derived from the ID.
// * If not, only the cluster is retrieved from compute_cluster_id. -1 is
// returned for the key.
func resourceVSphereComputeClusterVMHostRuleObjects(
	d *schema.ResourceData,
	meta interface{},
) (*object.ClusterComputeResource, int32, error) {
	if d.Id() != "" {
		return resourceVSphereComputeClusterVMHostRuleObjectsFromID(d, meta)
	}
	return resourceVSphereComputeClusterVMHostRuleObjectsFromAttributes(d, meta)
}

func resourceVSphereComputeClusterVMHostRuleObjectsFromAttributes(
	d *schema.ResourceData,
	meta interface{},
) (*object.ClusterComputeResource, int32, error) {
	return resourceVSphereComputeClusterVMHostRuleFetchObjects(
		meta,
		d.Get("compute_cluster_id").(string),
		-1,
	)
}

func resourceVSphereComputeClusterVMHostRuleObjectsFromID(
	d structure.ResourceIDStringer,
	meta interface{},
) (*object.ClusterComputeResource, int32, error) {
	// Note that this function uses structure.ResourceIDStringer to satisfy
	// interfacer. Adding exceptions in the comments does not seem to work.
	// Change this back to ResourceData if it's needed in the future.
	clusterID, key, err := resourceVSphereComputeClusterVMHostRuleParseID(d.Id())
	if err != nil {
		return nil, 0, err
	}

	return resourceVSphereComputeClusterVMHostRuleFetchObjects(meta, clusterID, key)
}

// resourceVSphereComputeClusterVMHostRuleFetchObjects fetches the "objects"
// for a cluster rule. This is currently just the cluster object as the rule
// key a static value and a pass-through - this is to keep its workflow
// consistent with other cluster-dependent resources that derive from
// ArrayUpdateSpec that have managed object as keys, such as VM and host
// overrides.
func resourceVSphereComputeClusterVMHostRuleFetchObjects(
	meta interface{},
	clusterID string,
	key int32,
) (*object.ClusterComputeResource, int32, error) {
	client, err := resourceVSphereComputeClusterVMHostRuleClient(meta)
	if err != nil {
		return nil, 0, err
	}

	cluster, err := clustercomputeresource.FromID(client, clusterID)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot locate cluster: %s", err)
	}

	return cluster, key, nil
}

func resourceVSphereComputeClusterVMHostRuleClient(meta interface{}) (*govmomi.Client, error) {
	client := meta.(*Client).vimClient
	if err := viapi.ValidateVirtualCenter(client); err != nil {
		return nil, err
	}
	return client, nil
}
