/*
Copyright © 2020 The k3d Author(s)

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/
package cluster

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	k3drt "github.com/rancher/k3d/pkg/runtimes"
	k3d "github.com/rancher/k3d/pkg/types"
	"github.com/rancher/k3d/pkg/util"
	log "github.com/sirupsen/logrus"
)

// CreateCluster creates a new cluster consisting of
// - some containerized k3s nodes
// - a docker network
func CreateCluster(cluster *k3d.Cluster, runtime k3drt.Runtime) error {

	/*
	 * Network
	 */

	// error out if external cluster network should be used but no name was set
	if cluster.Network.Name == "" && cluster.Network.External {
		return fmt.Errorf("Failed to use external network because no name was specified")
	}

	// generate cluster network name, if not set
	if cluster.Network.Name == "" && !cluster.Network.External {
		cluster.Network.Name = fmt.Sprintf("%s-%s", k3d.DefaultObjectNamePrefix, cluster.Name)
	}

	// create cluster network or use an existing one
	networkID, networkExists, err := runtime.CreateNetworkIfNotPresent(cluster.Network.Name)
	if err != nil {
		log.Errorln("Failed to create cluster network")
		return err
	}
	cluster.Network.Name = networkID
	extraLabels := map[string]string{
		"k3d.cluster.network":          networkID,
		"k3d.cluster.network.external": strconv.FormatBool(cluster.Network.External),
	}
	if networkExists {
		extraLabels["k3d.cluster.network.external"] = "true" // if the network wasn't created, we say that it's managed externally (important for cluster deletion)
	}

	/*
	 * Cluster Secret
	 */

	if cluster.Secret == "" {
		cluster.Secret = GenerateClusterSecret()
	}

	/*
	 * Cluster-Wide volumes
	 * - image volume (for importing images)
	 */
	if !cluster.CreateClusterOpts.DisableImageVolume {
		imageVolumeName := fmt.Sprintf("%s-%s-images", k3d.DefaultObjectNamePrefix, cluster.Name)
		if err := runtime.CreateVolume(imageVolumeName, map[string]string{"k3d.cluster": cluster.Name}); err != nil {
			log.Errorln("Failed to create image volume '%s' for cluster '%s'", imageVolumeName, cluster.Name)
			return err
		}

		extraLabels["k3d.cluster.imageVolume"] = imageVolumeName

		// attach volume to nodes
		for _, node := range cluster.Nodes {
			node.Volumes = append(node.Volumes, fmt.Sprintf("%s:%s", imageVolumeName, k3d.DefaultImageVolumeMountPath))
		}
	}

	/*
	 * Nodes
	 */

	nodeSetup := func(node *k3d.Node, suffix int) error {
		// cluster specific settings
		if node.Labels == nil {
			node.Labels = make(map[string]string) // TODO: maybe create an init function?
		}
		node.Labels["k3d.cluster"] = cluster.Name
		node.Env = append(node.Env, fmt.Sprintf("K3S_TOKEN=%s", cluster.Secret))
		node.Labels["k3d.cluster.secret"] = cluster.Secret

		// append extra labels
		for k, v := range extraLabels {
			node.Labels[k] = v
		}

		// node role specific settings
		if node.Role == k3d.MasterRole {

			// the cluster has an init master node, but its not this one, so connect it to the init node
			if cluster.InitNode != nil && !node.MasterOpts.IsInit {
				node.Args = append(node.Args, "--server", fmt.Sprintf("https://%s:%d", cluster.InitNode.Name, 6443))
			}

		} else if node.Role == k3d.WorkerRole {
			// connection url
			connectionURL := fmt.Sprintf("https://%s:%d", generateNodeName(cluster.Name, k3d.MasterRole, 0), 6443)
			node.Env = append(node.Env, fmt.Sprintf("K3S_URL=%s", connectionURL))
			node.Labels["k3d.cluster.url"] = connectionURL
		}

		node.Name = generateNodeName(cluster.Name, node.Role, suffix)
		node.Network = cluster.Network.Name

		// create node
		log.Infof("Creating node '%s'", node.Name)
		if err := CreateNode(node, runtime); err != nil {
			log.Errorln("Failed to create node")
			return err
		}
		log.Debugf("Created node '%s'", node.Name)

		return err
	}

	// used for node suffices
	masterCount := 0
	workerCount := 0
	suffix := 0

	// create init node first
	if cluster.InitNode != nil {
		log.Infoln("Creating initializing master node")
		cluster.InitNode.Args = append(cluster.InitNode.Args, "--cluster-init")
		if err := nodeSetup(cluster.InitNode, masterCount); err != nil {
			return err
		}
		masterCount++

		// wait for the initnode to come up before doing anything else
		for {
			log.Debugln("Waiting for initializing master node...")
			logreader, err := runtime.GetNodeLogs(cluster.InitNode)
			defer logreader.Close()
			if err != nil {
				logreader.Close()
				log.Errorln(err)
				log.Errorln("Failed to get logs from the initializig master node.. waiting for 3 seconds instead")
				time.Sleep(3 * time.Second)
				break
			}
			buf := new(bytes.Buffer)
			nRead, _ := buf.ReadFrom(logreader)
			logreader.Close()
			if nRead > 0 && strings.Contains(buf.String(), "Running kubelet") {
				log.Debugln("Initializing master node is up... continuing")
				break
			}
			time.Sleep(time.Second) // TODO: timeout
		}

	}

	// vars to support waiting for master nodes to be ready
	var waitForMasterWaitgroup sync.WaitGroup
	waitForMasterErrChan := make(chan error, 1) // FIXME: we should do something different here, e.g. concurrently read from the channel and append to a slice of errors

	// create all other nodes, but skip the init node
	for _, node := range cluster.Nodes {
		if node.Role == k3d.MasterRole {

			time.Sleep(1) // FIXME: arbitrary wait for one second to avoid race conditions of masters registering
			// skip the init node here
			if node == cluster.InitNode {
				continue
			}

			// name suffix
			suffix = masterCount
			masterCount++

		} else if node.Role == k3d.WorkerRole {
			// name suffix
			suffix = workerCount
			workerCount++
		}
		if err := nodeSetup(node, suffix); err != nil {
			return err
		}

		// asynchronously wait for this master node to be ready (by checking the logs for a specific log mesage)
		if node.Role == k3d.MasterRole && cluster.CreateClusterOpts.WaitForMaster >= 0 {
			waitForMasterWaitgroup.Add(1)
			go func(masterNode *k3d.Node) {
				log.Debugf("Starting to wait for master node '%s'", masterNode.Name)
				// TODO: it may be better to give endtime=starttime+timeout here so that there is no difference between the instances (go func may be called with a few (milli-)seconds difference)
				err := WaitForNodeLogMessage(runtime, masterNode, "Wrote kubeconfig", (time.Duration(cluster.CreateClusterOpts.WaitForMaster) * time.Second))
				waitForMasterErrChan <- err
				if err == nil {
					log.Debugf("Master Node '%s' ready", masterNode.Name)
				}
			}(node)
		}
	}

	// block until all masters are ready (if --wait was set) and collect errors if not
	if cluster.CreateClusterOpts.WaitForMaster >= 0 {
		errs := []error{}
		go func() {
			for elem := range waitForMasterErrChan {
				if elem != nil {
					errs = append(errs, elem)
				}
				waitForMasterWaitgroup.Done()
			}
		}()
		waitForMasterWaitgroup.Wait()
		if len(errs) != 0 {
			log.Errorln("Failed to bring up all master nodes in time. Check the logs:")
			for _, e := range errs {
				log.Errorln(">>> ", e)
			}
			return fmt.Errorf("Failed to bring up cluster") // TODO: in case of failure, we should rollback
		}
	}

	return nil
}

// DeleteCluster deletes an existing cluster
func DeleteCluster(cluster *k3d.Cluster, runtime k3drt.Runtime) error {

	log.Infof("Deleting cluster '%s'", cluster.Name)
	log.Debugf("%+v", cluster)

	failed := 0
	for _, node := range cluster.Nodes {
		if err := runtime.DeleteNode(node); err != nil {
			log.Warningf("Failed to delete node '%s': Try to delete it manually", node.Name)
			failed++
			continue
		}
	}

	// Delete the cluster network, if it was created for/by this cluster (and if it's not in use anymore)
	if cluster.Network.Name != "" {
		if !cluster.Network.External {
			log.Infof("Deleting cluster network '%s'", cluster.Network.Name)
			if err := runtime.DeleteNetwork(cluster.Network.Name); err != nil {
				if strings.HasSuffix(err.Error(), "active endpoints") {
					log.Warningf("Failed to delete cluster network '%s' because it's still in use: is there another cluster using it?", cluster.Network.Name)
				} else {
					log.Warningf("Failed to delete cluster network '%s': '%+v'", cluster.Network.Name, err)
				}
			}
		} else if cluster.Network.External {
			log.Debugf("Skip deletion of cluster network '%s' because it's managed externally", cluster.Network.Name)
		}
	}

	// delete image volume
	if cluster.ImageVolume != "" {
		log.Infof("Deleting image volume '%s'", cluster.ImageVolume)
		if err := runtime.DeleteVolume(cluster.ImageVolume); err != nil {
			log.Warningf("Failed to delete image volume '%s' of cluster '%s': Try to delete it manually", cluster.ImageVolume, cluster.Name)
		}
	}

	// return error if we failed to delete a node
	if failed > 0 {
		return fmt.Errorf("Failed to delete %d nodes: Try to delete them manually", failed)
	}
	return nil
}

// GetClusters returns a list of all existing clusters
func GetClusters(runtime k3drt.Runtime) ([]*k3d.Cluster, error) {
	nodes, err := runtime.GetNodesByLabel(k3d.DefaultObjectLabels)
	if err != nil {
		log.Errorln("Failed to get clusters")
		return nil, err
	}

	clusters := []*k3d.Cluster{}
	// for each node, check, if we can add it to a cluster or add the cluster if it doesn't exist yet
	for _, node := range nodes {
		clusterExists := false
		for _, cluster := range clusters {
			if node.Labels["k3d.cluster"] == cluster.Name { // TODO: handle case, where this label doesn't exist
				cluster.Nodes = append(cluster.Nodes, node)
				clusterExists = true
				break
			}
		}
		// cluster is not in the list yet, so we add it with the current node as its first member
		if !clusterExists {
			clusters = append(clusters, &k3d.Cluster{
				Name:  node.Labels["k3d.cluster"],
				Nodes: []*k3d.Node{node},
			})
		}
	}

	// enrich cluster structs with label values
	for _, cluster := range clusters {
		if err := populateClusterFieldsFromLabels(cluster); err != nil {
			log.Warnf("Failed to populate cluster fields from node label values for cluster '%s'", cluster.Name)
			log.Warnln(err)
		}
	}
	return clusters, nil
}

// populateClusterFieldsFromLabels inspects labels attached to nodes and translates them to struct fields
func populateClusterFieldsFromLabels(cluster *k3d.Cluster) error {
	networkExternalSet := false

	for _, node := range cluster.Nodes {

		// get the name of the cluster network
		if cluster.Network.Name == "" {
			if networkName, ok := node.Labels["k3d.cluster.network"]; ok {
				cluster.Network.Name = networkName
			}
		}

		// check if the network is external
		// since the struct value is a bool, initialized as false, we cannot check if it's unset
		if !cluster.Network.External && !networkExternalSet {
			if networkExternalString, ok := node.Labels["k3d.cluster.network.external"]; ok {
				if networkExternal, err := strconv.ParseBool(networkExternalString); err == nil {
					cluster.Network.External = networkExternal
					networkExternalSet = true
				}
			}
		}

		// get image volume // TODO: enable external image volumes the same way we do it with networks
		if cluster.ImageVolume == "" {
			if imageVolumeName, ok := node.Labels["k3d.cluster.imageVolume"]; ok {
				cluster.ImageVolume = imageVolumeName
			}
		}

	}

	return nil
}

// GetCluster returns an existing cluster
func GetCluster(cluster *k3d.Cluster, runtime k3drt.Runtime) (*k3d.Cluster, error) {
	// get nodes that belong to the selected cluster
	nodes, err := runtime.GetNodesByLabel(map[string]string{"k3d.cluster": cluster.Name})
	if err != nil {
		log.Errorf("Failed to get nodes for cluster '%s'", cluster.Name)
	}

	if len(nodes) == 0 {
		return nil, fmt.Errorf("No nodes found for cluster '%s'", cluster.Name)
	}

	// append nodes
	for _, node := range nodes {
		cluster.Nodes = append(cluster.Nodes, node)
	}

	if err := populateClusterFieldsFromLabels(cluster); err != nil {
		log.Warnf("Failed to populate cluster fields from node labels")
		log.Warnln(err)
	}

	return cluster, nil
}

// GenerateClusterSecret generates a random 20 character string
func GenerateClusterSecret() string {
	return util.GenerateRandomString(20)
}

func generateNodeName(cluster string, role k3d.Role, suffix int) string {
	return fmt.Sprintf("%s-%s-%s-%d", k3d.DefaultObjectNamePrefix, cluster, role, suffix)
}

// StartCluster starts a whole cluster (i.e. all nodes of the cluster)
func StartCluster(cluster *k3d.Cluster, runtime k3drt.Runtime) error {
	log.Infof("Starting cluster '%s'", cluster.Name)

	failed := 0
	for _, node := range cluster.Nodes {
		if err := runtime.StartNode(node); err != nil {
			log.Warningf("Failed to start node '%s': Try to start it manually", node.Name)
			failed++
			continue
		}
	}

	if failed > 0 {
		return fmt.Errorf("Failed to start %d nodes: Try to start them manually", failed)
	}
	return nil
}

// StopCluster stops a whole cluster (i.e. all nodes of the cluster)
func StopCluster(cluster *k3d.Cluster, runtime k3drt.Runtime) error {
	log.Infof("Stopping cluster '%s'", cluster.Name)

	failed := 0
	for _, node := range cluster.Nodes {
		if err := runtime.StopNode(node); err != nil {
			log.Warningf("Failed to stop node '%s': Try to stop it manually", node.Name)
			failed++
			continue
		}
	}

	if failed > 0 {
		return fmt.Errorf("Failed to stop %d nodes: Try to stop them manually", failed)
	}
	return nil
}
