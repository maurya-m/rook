/*
Copyright 2016 The Rook Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mon

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rook/rook/pkg/operator/ceph/config"
	"github.com/rook/rook/pkg/operator/k8sutil"

	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	rookalpha "github.com/rook/rook/pkg/apis/rook.io/v1alpha2"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/daemon/ceph/client"
	clienttest "github.com/rook/rook/pkg/daemon/ceph/client/test"
	cephtest "github.com/rook/rook/pkg/daemon/ceph/test"
	"github.com/rook/rook/pkg/operator/test"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// generate a standard mon config from a mon id w/ default port and IP 2.4.6.{1,2,3,...}
// support mon ID as new ["a", "b", etc.] form or as legacy ["mon0", "mon1", etc.] form
func testGenMonConfig(monID string) *monConfig {
	var moniker string
	var index int
	var err error
	if strings.HasPrefix(monID, "mon") { // is legacy mon name
		moniker = monID                                                 // keep legacy "mon#" name
		index, err = strconv.Atoi(strings.Replace(monID, "mon", "", 1)) // get # off end of mon#
	} else {
		moniker = "mon-" + monID
		index, err = k8sutil.NameToIndex(monID)
	}
	if err != nil {
		panic(err)
	}
	return &monConfig{
		ResourceName: "rook-ceph-" + moniker, // rook-ceph-mon-A or rook-ceph-mon#
		DaemonName:   monID,                  // A or mon#
		Port:         DefaultPort,
		PublicIP:     fmt.Sprintf("2.4.6.%d", index+1),
		// dataDirHostPath assumed to be /var/lib/rook
		DataPathMap: config.NewStatefulDaemonDataPathMap(
			"/var/lib/rook", dataDirRelativeHostPath(monID), config.MonType, monID),
	}
}

func newTestStartCluster(namespace string) *clusterd.Context {
	clientset := test.New(3)
	configDir, _ := ioutil.TempDir("", "")
	defer os.RemoveAll(configDir)
	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(debug bool, actionName string, command string, args ...string) (string, error) {
			if strings.Contains(command, "ceph-authtool") {
				cephtest.CreateConfigDir(path.Join(configDir, namespace))
			}
			return "", nil
		},
		MockExecuteCommandWithOutputFile: func(debug bool, actionName string, command string, outFileArg string, args ...string) (string, error) {
			// mock quorum health check because a second `Start()` triggers a health check
			return clienttest.MonInQuorumResponseMany(3), nil
		},
	}
	return &clusterd.Context{
		Clientset: clientset,
		Executor:  executor,
		ConfigDir: configDir,
	}
}

func newCluster(context *clusterd.Context, namespace string, hostNetwork bool, allowMultiplePerNode bool, resources v1.ResourceRequirements) *Cluster {
	return &Cluster{
		HostNetwork:          hostNetwork,
		context:              context,
		Namespace:            namespace,
		rookVersion:          "myversion",
		Count:                3,
		AllowMultiplePerNode: allowMultiplePerNode,
		maxMonID:             -1,
		waitForStart:         false,
		monPodRetryInterval:  10 * time.Millisecond,
		monPodTimeout:        1 * time.Second,
		monTimeoutList:       map[string]time.Time{},
		mapping: &Mapping{
			Node: map[string]*NodeInfo{},
			Port: map[string]int32{},
		},
		resources: resources,
		ownerRef:  metav1.OwnerReference{},
	}
}

func TestResourceName(t *testing.T) {
	assert.Equal(t, "rook-ceph-mon-a", resourceName("rook-ceph-mon-a"))
	assert.Equal(t, "rook-ceph-mon123", resourceName("rook-ceph-mon123"))
	assert.Equal(t, "rook-ceph-mon-b", resourceName("b"))
}

func TestStartMonPods(t *testing.T) {
	namespace := "ns"
	context := newTestStartCluster(namespace)
	c := newCluster(context, namespace, false, true, v1.ResourceRequirements{})

	// start a basic cluster
	err := c.Start()
	assert.Nil(t, err)

	validateStart(t, c)

	// starting again should be a no-op, but still results in an error
	err = c.Start()
	assert.Nil(t, err)

	validateStart(t, c)
}

func TestOperatorRestart(t *testing.T) {
	namespace := "ns"
	context := newTestStartCluster(namespace)
	c := newCluster(context, namespace, false, true, v1.ResourceRequirements{})
	c.clusterInfo = test.CreateConfigDir(1)

	// start a basic cluster
	err := c.Start()
	assert.Nil(t, err)

	validateStart(t, c)

	c = newCluster(context, namespace, false, true, v1.ResourceRequirements{})

	// starting again should be a no-op, but will not result in an error
	err = c.Start()
	assert.Nil(t, err)

	validateStart(t, c)
}

// safety check that if hostNetwork is used no changes occur on an operator restart
func TestOperatorRestartHostNetwork(t *testing.T) {
	namespace := "ns"
	context := newTestStartCluster(namespace)

	// cluster without host networking
	c := newCluster(context, namespace, false, false, v1.ResourceRequirements{})
	c.clusterInfo = test.CreateConfigDir(1)

	// start a basic cluster
	err := c.Start()
	assert.Nil(t, err)

	validateStart(t, c)

	// cluster with host networking
	c = newCluster(context, namespace, true, false, v1.ResourceRequirements{})

	// starting again should be a no-op, but still results in an error
	err = c.Start()
	assert.Nil(t, err)

	validateStart(t, c)
}

func validateStart(t *testing.T, c *Cluster) {
	s, err := c.context.Clientset.CoreV1().Secrets(c.Namespace).Get(appName, metav1.GetOptions{})
	assert.Nil(t, err) // there shouldn't be an error due the secret existing
	assert.Equal(t, 4, len(s.StringData))

	// there is only one pod created. the other two won't be created since the first one doesn't start
	_, err = c.context.Clientset.Extensions().Deployments(c.Namespace).Get("rook-ceph-mon-a", metav1.GetOptions{})
	assert.Nil(t, err)
}

func TestSaveMonEndpoints(t *testing.T) {
	clientset := test.New(1)
	configDir, _ := ioutil.TempDir("", "")
	defer os.RemoveAll(configDir)
	c := New(&clusterd.Context{Clientset: clientset, ConfigDir: configDir}, "ns", "", "myversion", cephv1.CephVersionSpec{},
		cephv1.MonSpec{Count: 3, AllowMultiplePerNode: true}, rookalpha.Placement{}, false,
		v1.ResourceRequirements{}, metav1.OwnerReference{})
	c.clusterInfo = test.CreateConfigDir(1)

	// create the initial config map
	err := c.saveMonConfig()
	assert.Nil(t, err)

	cm, err := c.context.Clientset.CoreV1().ConfigMaps(c.Namespace).Get(EndpointConfigMapName, metav1.GetOptions{})
	assert.Nil(t, err)
	assert.Equal(t, "a=1.2.3.1:6789", cm.Data[EndpointDataKey])
	assert.Equal(t, `{"node":{},"port":{}}`, cm.Data[MappingKey])
	assert.Equal(t, "-1", cm.Data[MaxMonIDKey])

	// update the config map
	c.clusterInfo.Monitors["a"].Endpoint = "2.3.4.5:6789"
	c.maxMonID = 2
	c.mapping.Node["a"] = &NodeInfo{
		Name:     "node0",
		Address:  "1.1.1.1",
		Hostname: "myhost",
	}
	c.mapping.Port["node0"] = int32(12345)
	err = c.saveMonConfig()
	assert.Nil(t, err)

	cm, err = c.context.Clientset.CoreV1().ConfigMaps(c.Namespace).Get(EndpointConfigMapName, metav1.GetOptions{})
	assert.Nil(t, err)
	assert.Equal(t, "a=2.3.4.5:6789", cm.Data[EndpointDataKey])
	assert.Equal(t, `{"node":{"a":{"Name":"node0","Hostname":"myhost","Address":"1.1.1.1"}},"port":{"node0":12345}}`, cm.Data[MappingKey])
	assert.Equal(t, "2", cm.Data[MaxMonIDKey])
}

func TestMonInQuorum(t *testing.T) {
	entry := client.MonMapEntry{Name: "foo", Rank: 23}
	quorum := []int{}
	// Nothing in quorum
	assert.False(t, monInQuorum(entry, quorum))

	// One or more members in quorum
	quorum = []int{23}
	assert.True(t, monInQuorum(entry, quorum))
	quorum = []int{5, 6, 7, 23, 8}
	assert.True(t, monInQuorum(entry, quorum))

	// Not in quorum
	entry.Rank = 1
	assert.False(t, monInQuorum(entry, quorum))
}

func TestNameToIndex(t *testing.T) {
	// invalid
	id, err := fullNameToIndex("m")
	assert.NotNil(t, err)
	assert.Equal(t, -1, id)
	id, err = fullNameToIndex("mon")
	assert.NotNil(t, err)
	assert.Equal(t, -1, id)
	id, err = fullNameToIndex("rook-ceph-monitor0")
	assert.NotNil(t, err)
	assert.Equal(t, -1, id)

	// valid
	id, err = fullNameToIndex("rook-ceph-mon-a")
	assert.Nil(t, err)
	assert.Equal(t, 0, id)
	id, err = fullNameToIndex("rook-ceph-mon123")
	assert.Nil(t, err)
	assert.Equal(t, 123, id)
}
