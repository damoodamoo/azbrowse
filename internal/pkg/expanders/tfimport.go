package expanders

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/hashicorp/terraform/configs/configschema"
	"github.com/hashicorp/terraform/providers"
	"github.com/zclconf/go-cty/cty"

	"github.com/lawrencegripper/azbrowse/internal/pkg/tfprovider"
	"github.com/lawrencegripper/azbrowse/internal/pkg/tracing"
	"github.com/lawrencegripper/azbrowse/pkg/armclient"
	"github.com/lawrencegripper/azbrowse/pkg/endpoints"
)

var vmEndpoint = endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Compute/virtualMachines/{vmName}", "2020-06-01")

// lookup mapping of resource type to regexp expression to match resource IDs
var tfimportBaseConfig = map[string]*endpoints.EndpointInfo{
	"azurerm_resource_group": endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionName}/resourceGroups/{resourceGroupName}", ""),

	// App Service
	"azurerm_app_service_plan": endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionName}/resourceGroups/{resourceGroupName}/providers/Microsoft.Web/serverfarms/{farmName}", ""),
	"azurerm_app_service":      endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionName}/resourceGroups/{resourceGroupName}/providers/Microsoft.Web/sites/{siteName}", ""),

	// Storage
	"azurerm_storage_account": endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionName}/resourceGroups/{resourceGroupName}/providers/Microsoft.Storage/storageAccounts/{accountName}", ""),
	// TODO - add a check to the storage account and conditionally use azurerm_storage_data_lake_gen2_filesystem instead of azurerm_storage_container if isHnsEnabled is true
	"azurerm_storage_container": endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionName}/resourceGroups/{resourceGroupName}/providers/Microsoft.Storage/storageAccounts/{accountName}/blobServices/default/containers/{containerName}", ""),

	// SQL Database
	"azurerm_mssql_server":   endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionName}/resourceGroups/{resourceGroupName}/providers/Microsoft.Sql/servers/{serverName}", ""),
	"azurerm_mssql_database": endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Sql/servers/{serverName}/databases/{databaseName}", ""),

	// Networking
	"azurerm_private_endpoint":                      endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionName}/resourceGroups/{resourceGroupName}/providers/Microsoft.Network/privateEndpoints/{endpointName}", ""),
	"azurerm_network_interface":                     endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Network/networkInterfaces/{networkInterfaceName}", ""),
	"azurerm_network_security_group":                endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Network/networkSecurityGroups/{networkSecurityGroupName}", ""),
	"azurerm_network_security_rule":                 endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Network/networkSecurityGroups/{networkSecurityGroupName}/securityRules/{securityRuleName}", ""),
	"azurerm_private_dns_zone":                      endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Network/privateDnsZones/{privateZoneName}", ""),
	"azurerm_private_dns_zone_virtual_network_link": endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Network/privateDnsZones/{privateZoneName}/virtualNetworkLinks/{virtualNetworkLinkName}", ""),
	"azurerm_public_ip":                             endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Network/publicIPAddresses/{publicIpAddressName}", ""),
	"azurerm_virtual_network_gateway":               endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Network/virtualNetworkGateways/{virtualNetworkGatewayName}", ""),
	"azurerm_virtual_network":                       endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Network/virtualNetworks/{virtualNetworkName}", ""),
	"azurerm_subnet":                                endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Network/virtualNetworks/{virtualNetworkName}/subnets/{subnetName}", ""),

	// KeyVault
	"azurerm_key_vault": endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.KeyVault/vaults/{vaultName}", ""),

	// Virtual machines
	"azurerm_virtual_machine_extension": endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Compute/virtualMachines/{vmName}/extensions/{vmExtensionName}", ""),
	"azurerm_managed_disk":              endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Compute/disks/{diskName}", ""),
	"azurerm_windows_virtual_machine":   vmEndpoint,
	"azurerm_linux_virtual_machine":     vmEndpoint,

	// "":              endpoints.MustGetEndpointInfoFromURL("", ""),
}

// Global ignore rules based on resource ID suffix
var resourceIDIgnoreSuffices = []string{
	"/<diagsettings>",
	"/<activitylog>",
	"/providers/Microsoft.Resources/deployments",
	"/providers/microsoft.Insights/metrics",
	"/providers/microsoft.insights/metricdefinitions",
}

// Rules to ignore resource IDs, specified as EndpointInfos keyed on the parent resource type
var endpointsToIgnore = map[string][]*endpoints.EndpointInfo{
	"azurerm_app_service": {
		endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionName}/resourceGroups/{resourceGroupName}/providers/Microsoft.Web/sites/{siteName}/instances", ""),
		endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionName}/resourceGroups/{resourceGroupName}/providers/Microsoft.Web/sites/{siteName}/processes", ""),
	},
	"azurerm_storage_container": {
		endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionName}/resourceGroups/{resourceGroupName}/providers/Microsoft.Storage/storageAccounts/{accountName}/blobServices/default/containers/{containerName}/{path}", ""),
	},
	"azurerm_mssql_database": {
		endpoints.MustGetEndpointInfoFromURL("/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Sql/servers/{serverName}/databases/{databaseName}/{placeholder}", ""),
	},
}

// EndpointMatchTOResourceIDFunc is used to override the ID used for importing a resource
type EndpointMatchToResourceIDFunc func(matchingEndpoint *endpoints.EndpointInfo, matchValues map[string]string) (string, error)

var defaultEndpointMatchTOResourceIDFunc = func(matchingEndpoint *endpoints.EndpointInfo, matchValues map[string]string) (string, error) {
	return matchingEndpoint.BuildURL(matchValues)
}

// Map of functions to use to override the import URL if it needs to be different to the TreeNode ID
var resourceIDOverrides = map[string]EndpointMatchToResourceIDFunc{
	"azurerm_storage_container": func(matchingEndpoint *endpoints.EndpointInfo, matchValues map[string]string) (string, error) {
		overriddenEndpoint, err := endpoints.GetEndpointInfoFromURL("/{containerName}", "")
		if err != nil {
			return "", err
		}
		path, err := overriddenEndpoint.BuildURL(matchValues)
		if err != nil {
			return "", err
		}
		if accountName, ok := matchValues["accountName"]; ok {
			return fmt.Sprintf("https://%s.blob.core.windows.net%s", accountName, path), nil
		}
		return "", fmt.Errorf("accountName not found in match values")
	},
}

// TODO
//  - expand the list of mapped resource types
//  - handle Azure data-plane resources (e.g. storage containers)
//  - handle non-Azure resources? (e.g. databricks cluster)

const (
	tfimportActionGetTerraform          = "GetTerraform"
	tfimportActionGetTerraformRecursive = "GetTerraformRecursive"
)

// NewTerraformImportExpander creates a new instance of TerraformImportExpander
func NewTerraformImportExpander(armclient *armclient.Client) *TerraformImportExpander {
	return &TerraformImportExpander{
		nullLogger: tfprovider.NewNullLogger(),
		client:     armclient,
	}
}

// Check interface
var _ Expander = &TerraformImportExpander{}

// TerraformImportExpander provides actions
type TerraformImportExpander struct {
	ExpanderBase
	tfProvider *tfprovider.TerraformProvider
	nullLogger logr.Logger
	client     *armclient.Client
}

func (e *TerraformImportExpander) setClient(c *armclient.Client) {
	e.client = c
}

// Name returns the name of the expander
func (e *TerraformImportExpander) Name() string {
	return "TerraformImportExpander"
}

func (e *TerraformImportExpander) ensureTfProviderInitialized(context context.Context) error {
	if e.tfProvider != nil {
		return nil
	}

	// Get a provider instance by installing or using existing binary
	azbPath := "/root/.azbrowse/terraform/"
	user, err := user.Current()
	if err == nil {
		azbPath = user.HomeDir + "/.azbrowse/terraform/"
	}
	err = os.MkdirAll(azbPath, 0777)
	if err != nil {
		return err
	}

	config := tfprovider.TerraformProviderConfig{
		ProviderName:      "azurerm",
		ProviderVersion:   "2.38.0",
		ProviderConfigHCL: "features {}",
		ProviderPath:      azbPath,
	}
	provider, err := tfprovider.SetupProvider(context, e.nullLogger, config) // TODO - update to use azbrowse profile folder as cache
	if err != nil {
		return err
	}
	e.tfProvider = provider
	return nil
}

func (e *TerraformImportExpander) getResourceTypeNameFromResourceID(context context.Context, resourceID string) (string, error) {
	result := vmEndpoint.Match(resourceID)
	if result.IsMatch {
		body, err := e.client.DoRequest(context, "GET", resourceID+"?api-version="+vmEndpoint.APIVersion)
		if err != nil {
			return "", err
		}
		value, err := getJSONPropertyFromString(body, "properties", "storageProfile", "osDisk", "osType")
		if err != nil {
			return "", err
		}
		osType, ok := value.(string)
		if !ok {
			return "", err
		}
		switch osType {
		case "Windows":
			return "azurerm_windows_virtual_machine", nil
		case "Linux":
			return "azurerm_linux_virtual_machine", nil
		}
	}

	for resourceTypeName, resourceEndpoint := range tfimportBaseConfig {
		result := resourceEndpoint.Match(resourceID)
		if result.IsMatch {
			return resourceTypeName, nil
		}
	}
	return "", nil
}

func getJSONPropertyFromString(jsonString string, properties ...string) (interface{}, error) {
	var jsonData map[string]interface{}

	if err := json.Unmarshal([]byte(jsonString), &jsonData); err != nil {
		return nil, err
	}

	return getJSONProperty(jsonData, properties...)
}
func getJSONProperty(jsonData interface{}, properties ...string) (interface{}, error) {
	switch jsonData := jsonData.(type) {
	case map[string]interface{}:
		jsonMap := jsonData
		name := properties[0]
		jsonSubtree, ok := jsonMap[name]
		if ok {
			if len(properties) == 1 {
				return jsonSubtree, nil
			}
			return getJSONProperty(jsonSubtree, properties[1:]...)
		} else {
			return nil, nil // TODO - error if not found?
		}
	default:
		return nil, nil // TODO - error if not able to walk the tree?
	}

}

// HasActions is a default implementation returning false to indicate no actions available
func (e *TerraformImportExpander) HasActions(context context.Context, item *TreeNode) (bool, error) {
	resourceTypeName, err := e.getResourceTypeNameFromResourceID(context, item.ID)
	if err != nil {
		return false, err
	}
	if resourceTypeName == "" {
		return false, nil
	}
	if item.Metadata == nil {
		item.Metadata = map[string]string{}
	}
	item.Metadata["TerraformImportExpander_ResourceTypeName"] = resourceTypeName // cache to avoid repeating lookup (avoids ARM call in VM case)
	return true, nil
}

// ListActions returns an error as it should not be called as HasActions returns false
func (e *TerraformImportExpander) ListActions(context context.Context, item *TreeNode) ListActionsResult {

	resourceTypeName := item.Metadata["TerraformImportExpander_ResourceTypeName"]
	if resourceTypeName == "" {
		return ListActionsResult{
			SourceDescription: "TerraformImportExpander",
			Err:               fmt.Errorf("ResourceTypeName not set"),
		}
	}

	timeoutOverride := 300

	nodes := []*TreeNode{
		{
			Parentid:              item.ID,
			ID:                    item.ID + "?" + tfimportActionGetTerraform,
			Namespace:             "tfimport",
			Name:                  "Get Terraform",
			Display:               "Get Terraform",
			ItemType:              ActionType,
			SuppressGenericExpand: true,
			Metadata: map[string]string{
				"ActionID": tfimportActionGetTerraform,
				"TerraformImportExpander_ResourceTypeName": resourceTypeName,
			},
		},
		{
			Parentid:              item.ID,
			ID:                    item.ID + "?" + tfimportActionGetTerraformRecursive,
			Namespace:             "tfimport",
			Name:                  "Get Terraform (recursive)",
			Display:               "Get Terraform (recursive)",
			ItemType:              ActionType,
			SuppressGenericExpand: true,
			Metadata: map[string]string{
				"ActionID": tfimportActionGetTerraformRecursive,
				"TerraformImportExpander_ResourceTypeName": resourceTypeName,
			},
			TimeoutOverrideSeconds: &timeoutOverride,
		},
	}
	return ListActionsResult{
		Nodes:             nodes,
		SourceDescription: "TerraformImportExpander",
	}
}

// ExecuteAction returns an error as it should not be called as HasActions returns false
func (e *TerraformImportExpander) ExecuteAction(context context.Context, item *TreeNode) ExpanderResult {
	actionID := item.Metadata["ActionID"]

	var err error
	switch actionID {
	case tfimportActionGetTerraform:
		resourceTypeName := item.Metadata["TerraformImportExpander_ResourceTypeName"]
		if resourceTypeName == "" {
			return ExpanderResult{
				SourceDescription: "TerraformImportExpander",
				Err:               fmt.Errorf("ResourceTypeName not set"),
			}
		}
		return e.getTerraformForNode(context, item.Parent) // Item refers to the Action node - it's parent is the node it is the action for
	case tfimportActionGetTerraformRecursive:
		return e.getTerraformForNodeRecursive(context, item.Parent, 50, "") // Item refers to the Action node - it's parent is the node it is the action for
	case "":
		err = fmt.Errorf("ActionID metadata not set: %q", item.ID)
	default:
		err = fmt.Errorf("Unhandled ActionID: %q", actionID)
	}
	return ExpanderResult{
		SourceDescription: "TerraformImportExpander",
		Err:               err,
	}
}
func (e *TerraformImportExpander) getTerraformForNodeRecursive(context context.Context, item *TreeNode, remainingDepth int, lastResourceTypeName string) ExpanderResult {

	// Get Terraform for the current node...
	terraform := ""
	result := e.getTerraformForNode(context, item)
	if result.Err != nil {
		terraform = fmt.Sprintf("%s\n#Error: %s", terraform, result.Err)
	} else {
		terraform = fmt.Sprintf("%s\n%s", terraform, result.Response.Response)
	}

	if remainingDepth > 0 { // TODO - need to figure out a better way to avoid a massive tree crawl!
		_, childItems, err := ExpandItemAllowDefaultExpander(context, item, false)
		if err != nil {
			terraform = fmt.Sprintf("%s\n#Error expanding %q: %s", terraform, item.ID, err)
			return ExpanderResult{
				SourceDescription: "TerraformImportExpander",
				Response: ExpanderResponse{
					ResponseType: ResponseTerraform,
					Response:     terraform,
				},
				IsPrimaryResponse: true,
			}
		}

		for _, childItem := range childItems {
			ignore := false
			for _, suffix := range resourceIDIgnoreSuffices {
				if strings.HasSuffix(childItem.ID, suffix) {
					ignore = true
					break
				}
			}
			if ignore {
				continue
			}

			resourceTypeName := item.Metadata["TerraformImportExpander_ResourceTypeName"]
			if resourceTypeName == "" {
				resourceTypeName = lastResourceTypeName
			}
			ignoreRules := endpointsToIgnore[resourceTypeName]
			for _, ignoreEndpoint := range ignoreRules {
				result := ignoreEndpoint.Match(childItem.ID)
				if result.IsMatch {
					ignore = true
					break
				}
			}
			if ignore {
				continue
			}

			result = e.getTerraformForNodeRecursive(context, childItem, remainingDepth-1, lastResourceTypeName)
			if result.Err != nil {
				terraform = fmt.Sprintf("%s\n#Error: %s", terraform, result.Err)
			} else {
				terraform = fmt.Sprintf("%s%s", terraform, result.Response.Response)
			}
		}
	}

	return ExpanderResult{
		SourceDescription: "TerraformImportExpander",
		Response: ExpanderResponse{
			ResponseType: ResponseTerraform,
			Response:     terraform,
		},
		IsPrimaryResponse: true,
	}
}

func (e *TerraformImportExpander) getTerraformForNode(context context.Context, item *TreeNode) ExpanderResult {
	span, context := tracing.StartSpanFromContext(context, "terraform:get-for-node:"+item.ItemType+":"+item.Name, tracing.SetTag("item", item))
	defer span.Finish()
	err := e.ensureTfProviderInitialized(context)
	if err != nil {
		return ExpanderResult{
			SourceDescription: "TerraformImportExpander",
			Err:               err,
		}
	}

	if item.Metadata == nil {
		item.Metadata = map[string]string{}
	}
	resourceTypeName := item.Metadata["TerraformImportExpander_ResourceTypeName"]
	if resourceTypeName == "" {
		resourceTypeName, err = e.getResourceTypeNameFromResourceID(context, item.ID)
		if err != nil {
			return ExpanderResult{
				SourceDescription: "TerraformImportExpander",
				Err:               err,
			}
		}
		item.Metadata["TerraformImportExpander_ResourceTypeName"] = resourceTypeName
	}
	if resourceTypeName == "" {
		return ExpanderResult{
			SourceDescription: "TerraformImportExpander",
			Err:               fmt.Errorf("No ResourceTypeName for %q", item.ID),
		}
	}

	id := item.ID
	endpoint := tfimportBaseConfig[resourceTypeName]
	if endpoint == nil {
		return ExpanderResult{
			SourceDescription: "TerraformImportExpander",
			Err:               fmt.Errorf("Endpoint not found for resourceTypeName %q", resourceTypeName),
		}
	}
	endpointMatch := endpoint.Match(id)
	if !endpointMatch.IsMatch {
		return ExpanderResult{
			SourceDescription: "TerraformImportExpander",
			Err:               fmt.Errorf("Failed to match resource type name"),
		}
	}
	getResourceIDFunc := resourceIDOverrides[resourceTypeName] // use override endpoint to build custom ID for import
	if getResourceIDFunc == nil {
		// use the endpoint to rebuild the ID URL to ensure the case matches what the azurerm provider expects
		getResourceIDFunc = defaultEndpointMatchTOResourceIDFunc
	}
	id, err = getResourceIDFunc(endpoint, endpointMatch.Values)
	if err != nil {
		return ExpanderResult{
			SourceDescription: "TerraformImportExpander",
			Err:               err,
		}
	}

	terraform, err := e.getTerraformFor(context, id, resourceTypeName)
	if err != nil {
		return ExpanderResult{
			SourceDescription: "TerraformImportExpander",
			Err:               err,
		}
	}
	return ExpanderResult{
		SourceDescription: "TerraformImportExpander",
		Response: ExpanderResponse{
			ResponseType: ResponseTerraform,
			Response:     terraform,
		},
		IsPrimaryResponse: true,
	}
}

func (e *TerraformImportExpander) getTerraformFor(context context.Context, id string, resourceTypeName string) (string, error) {
	span, context := tracing.StartSpanFromContext(context, "terraform:get-schema:"+resourceTypeName)
	defer span.Finish()
	terraformProviderSchema := e.tfProvider.Plugin.GetSchema()
	importRequest := providers.ImportResourceStateRequest{
		TypeName: resourceTypeName,
		ID:       id,
	}
	spanImport, context := tracing.StartSpanFromContext(context, "terraform:import:"+resourceTypeName)
	defer spanImport.Finish()
	importResponse := e.tfProvider.Plugin.ImportResourceState(importRequest)

	result := ""
	for _, resource := range importResponse.ImportedResources {
		span, _ := tracing.StartSpanFromContext(context, "terraform:read:"+resource.TypeName)
		readRequest := providers.ReadResourceRequest{
			TypeName:   resource.TypeName,
			PriorState: resource.State,
		}
		readResponse := e.tfProvider.Plugin.ReadResource(readRequest)
		span.Finish()

		resourceSchema := terraformProviderSchema.ResourceTypes[resource.TypeName]

		if readResponse.NewState.IsNull() {
			return "", fmt.Errorf("Null state on read for %q", id)
		}
		hclString, err := e.printState(readResponse.NewState, resource.TypeName, resourceSchema)
		if err != nil {
			return "", err
		}
		result += hclString
		result += "\n"
	}

	return result, nil
}

func (e *TerraformImportExpander) writeBlock(outputBlock *hclwrite.Block, terraformBlock *configschema.Block, state cty.Value) {
	// Sort attribute names:
	//    id
	//    name
	//    location
	//    resource_group_name
	//    <names ending in `_id`/`_ids`>
	//    <other names>
	keys := make([]string, 0, len(terraformBlock.Attributes))
	for k := range terraformBlock.Attributes {
		prefix := "z"
		switch {
		case k == "id":
			prefix = "a"
		case k == "name":
			prefix = "b"
		case k == "location":
			prefix = "c"
		case k == "resource_group_name":
			prefix = "d"
		case strings.HasSuffix(k, "_id") || strings.HasSuffix(k, "_ids"):
			prefix = "e"
		}
		keys = append(keys, prefix+k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		attributeName := k[1:]
		attributeSchema := terraformBlock.Attributes[attributeName]
		if !attributeSchema.Computed || attributeSchema.Optional {
			attributeValue := state.GetAttr(attributeName)
			outputBlock.Body().SetAttributeValue(attributeName, attributeValue)
		}
	}

	for blockName, blockSchema := range terraformBlock.BlockTypes {
		// TODO - might need to look at blockSchema.Nesting and handle accordingly
		newState := state.GetAttr(blockName)

		if newState.Type().IsObjectType() {
			newBlock := outputBlock.Body().AppendNewBlock(blockName, []string{})
			e.writeBlock(newBlock, &blockSchema.Block, newState)
		} else if newState.CanIterateElements() {
			iterator := newState.ElementIterator()
			for iterator.Next() {
				_, value := iterator.Element()
				if value.Type().IsObjectType() {
					newBlock := outputBlock.Body().AppendNewBlock(blockName, []string{})
					e.writeBlock(newBlock, &blockSchema.Block, value)
				}
			}
		} else {
			fmt.Printf("")
		}
	}
}
func (e *TerraformImportExpander) printState(state cty.Value, resourceTypeName string, schema providers.Schema) (string, error) {
	file := hclwrite.NewEmptyFile()
	terraformBlock := schema.Block
	name := "todo_resource_name"
	if state.Type().HasAttribute("name") {
		attribute := state.GetAttr("name")
		name = strings.ReplaceAll(attribute.AsString(), "-", "_")
	}
	block := file.Body().AppendNewBlock("resource", []string{resourceTypeName, name})
	e.writeBlock(block, terraformBlock, state)

	var buf bytes.Buffer
	_, err := file.WriteTo(&buf)
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}
