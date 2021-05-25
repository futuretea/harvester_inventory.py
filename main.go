package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	harvclient "github.com/harvester/harvester/pkg/generated/clientset/versioned"
	"github.com/rancher/wrangler/pkg/kubeconfig"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"
)

const (
	HostIPSourceType = "InternalIP"

	AnsibleVarHost = "ansible_host"
	AnsibleVarUser = "ansible_user"

	FieldHostVars = "hostvars"
	FieldMeta     = "_meta"
	FieldChildren = "children"
	FieldHosts    = "hosts"

	GroupAll             = "all"
	GroupNodes           = "nodes"
	GroupVirtualMachines = "vms"

	HarvesterSSHUserLabel = "harvesterhci.io/ssh-user"

	defaultSSHUser = "rancher"
)

type Host struct {
	Name        string
	IP          string
	Kind        string
	Labels      map[string]string
	Annotations map[string]string
}

type Inventory struct {
	Data map[string]interface{}
}

func (i *Inventory) AddGroupHosts(groupName string, hostNames ...string) {
	i.AddGroupFiled(groupName, FieldHosts, hostNames...)
}

func (i *Inventory) AddGroupChildren(groupName string, groupNames ...string) {
	i.AddGroupFiled(groupName, FieldChildren, groupNames...)
}

func (i *Inventory) AddGroupFiled(groupName string, filed string, names ...string) {
	var group map[string][]string
	if i.Data[groupName] == nil {
		group = map[string][]string{
			filed: {},
		}
	} else {
		group = i.Data[groupName].(interface{}).(map[string][]string)
	}
	group[filed] = append(group[filed], names...)
	i.Data[groupName] = group
}

func (i *Inventory) AddMetaHost(hostName string, hostVars ...map[string]string) {
	host := i.Data[FieldMeta].(interface{}).(map[string]map[string]map[string]string)[FieldHostVars][hostName]
	if host == nil {
		host = make(map[string]string)
	}
	for _, hostVar := range hostVars {
		for k, v := range hostVar {
			host[k] = v
		}
	}
	i.Data[FieldMeta].(map[string]map[string]map[string]string)[FieldHostVars][hostName] = host
}

func (i *Inventory) String() string {
	output, err := json.Marshal(i.Data)
	if err != nil {
		klog.Fatalln(err)
	}
	return string(output)
}

func NewInventory() *Inventory {
	return &Inventory{
		Data: map[string]interface{}{
			FieldMeta: map[string]map[string]map[string]string{
				FieldHostVars: {},
			},
			GroupAll: map[string][]string{
				FieldChildren: {},
			},
		},
	}
}

func generateGroupName(kind, prefix, key, value string) string {
	groupName := kind + "_" + prefix + "_" + key + "_" + value
	groupName = strings.ReplaceAll(groupName, ".", "_")
	groupName = strings.ReplaceAll(groupName, "-", "_")
	groupName = strings.ReplaceAll(groupName, "/", "_")
	return groupName
}

func autoGroupLabel(labelName string) bool {
	return strings.Contains(labelName, "kubernetes") || strings.Contains(labelName, "k3s") || strings.Contains(labelName, "harvester")
}

func parserHost(host Host) (map[string]string, []string) {
	vars := map[string]string{
		AnsibleVarHost: host.IP,
		AnsibleVarUser: defaultSSHUser,
	}

	if v, ok := host.Annotations[HarvesterSSHUserLabel]; ok {
		vars[AnsibleVarUser] = v
	}

	var groupNames = make([]string, 0, 1+len(host.Labels))
	switch host.Kind {
	case "node":
		groupNames = []string{GroupNodes}
	case "vm":
		groupNames = []string{GroupVirtualMachines}
	}
	for key, value := range host.Labels {
		if !autoGroupLabel(key) {
			continue
		}
		groupName := generateGroupName(host.Kind, "label", key, value)
		groupNames = append(groupNames, groupName)
	}
	return vars, groupNames
}

func buildInventory(hosts []Host) string {
	inventory := NewInventory()
	for _, host := range hosts {
		vars, groupNames := parserHost(host)
		inventory.AddMetaHost(host.Name, vars)
		for _, groupName := range groupNames {
			inventory.AddGroupHosts(groupName, host.Name)
		}
	}
	inventory.AddGroupChildren(GroupAll, GroupNodes, GroupVirtualMachines)
	return inventory.String()
}

func getAllVMs(harvClientSet *harvclient.Clientset) []Host {
	vmiList, err := harvClientSet.KubevirtV1().VirtualMachineInstances("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		klog.Fatalln(err)
	}

	var (
		ip     string
		kind   = "vm"
		result = make([]Host, 0, len(vmiList.Items))
	)

	for _, vmi := range vmiList.Items {
		for _, networkInterface := range vmi.Status.Interfaces {
			if networkInterface.IP != "" {
				ip = networkInterface.IP
				break
			}
		}
		result = append(result, Host{
			Kind:        kind,
			Name:        kind + "-" + vmi.Name,
			Labels:      vmi.Labels,
			Annotations: vmi.Annotations,
			IP:          ip,
		})
	}
	return result
}

func getAllNode(kubeClientSet *clientset.Clientset) []Host {
	nodeClient := kubeClientSet.CoreV1().Nodes()
	nodes, err := nodeClient.List(context.Background(), metav1.ListOptions{})
	if err != nil {
		klog.Fatalln(err)
	}

	var (
		kind   = "node"
		result = make([]Host, 0, len(nodes.Items))
	)

	for _, node := range nodes.Items {
		nodeIP := ""
		for _, address := range node.Status.Addresses {
			if address.Type == HostIPSourceType {
				nodeIP = address.Address
				break
			}
		}
		result = append(result, Host{
			Kind:        kind,
			Name:        kind + "-" + node.Name,
			Labels:      node.Labels,
			Annotations: node.Annotations,
			IP:          nodeIP,
		})
	}
	return result
}

func getRestConfig() *rest.Config {
	clientConfig := kubeconfig.GetNonInteractiveClientConfig("")
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		klog.Fatalln(err)
	}
	return restConfig
}

func main() {
	restConfig := getRestConfig()
	kubeClientSet := clientset.NewForConfigOrDie(restConfig)
	nodeList := getAllNode(kubeClientSet)
	harvClientSet := harvclient.NewForConfigOrDie(restConfig)
	vmiList := getAllVMs(harvClientSet)
	hosts := make([]Host, 0, len(nodeList)+len(vmiList))
	hosts = append(hosts, nodeList...)
	hosts = append(hosts, vmiList...)
	fmt.Println(buildInventory(hosts))
}
