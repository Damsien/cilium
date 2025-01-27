// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package clustermesh

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"testing"
	"time"

	. "github.com/cilium/checkmate"

	"github.com/cilium/cilium/pkg/clustermesh/internal"
	"github.com/cilium/cilium/pkg/clustermesh/types"
	cmtypes "github.com/cilium/cilium/pkg/clustermesh/types"
	cmutils "github.com/cilium/cilium/pkg/clustermesh/utils"
	"github.com/cilium/cilium/pkg/hive/hivetest"
	"github.com/cilium/cilium/pkg/identity"
	"github.com/cilium/cilium/pkg/identity/cache"
	"github.com/cilium/cilium/pkg/ipcache"
	"github.com/cilium/cilium/pkg/kvstore"
	"github.com/cilium/cilium/pkg/kvstore/store"
	"github.com/cilium/cilium/pkg/lock"
	nodeStore "github.com/cilium/cilium/pkg/node/store"
	fakeConfig "github.com/cilium/cilium/pkg/option/fake"
	"github.com/cilium/cilium/pkg/testutils"
	testidentity "github.com/cilium/cilium/pkg/testutils/identity"
)

func Test(t *testing.T) {
	TestingT(t)
}

type ClusterMeshTestSuite struct{}

var _ = Suite(&ClusterMeshTestSuite{})

func (s *ClusterMeshTestSuite) SetUpSuite(c *C) {
	testutils.IntegrationTest(c)
}

var (
	nodes      = map[string]*testNode{}
	nodesMutex lock.RWMutex
)

type testNode struct {
	// Name is the name of the node. This is typically the hostname of the node.
	Name string

	// Cluster is the name of the cluster the node is associated with
	Cluster string
}

func (n *testNode) GetKeyName() string {
	return path.Join(n.Cluster, n.Name)
}

func (n *testNode) DeepKeyCopy() store.LocalKey {
	return &testNode{
		Name:    n.Name,
		Cluster: n.Cluster,
	}
}

func (n *testNode) Marshal() ([]byte, error) {
	return json.Marshal(n)
}

func (n *testNode) Unmarshal(_ string, data []byte) error {
	return json.Unmarshal(data, n)
}

var testNodeCreator = func() store.Key {
	n := testNode{}
	return &n
}

type testObserver struct{}

func (o *testObserver) OnUpdate(k store.Key) {
	n := k.(*testNode)
	nodesMutex.Lock()
	nodes[n.GetKeyName()] = n
	nodesMutex.Unlock()
}

func (o *testObserver) OnDelete(k store.NamedKey) {
	n := k.(*testNode)
	nodesMutex.Lock()
	delete(nodes, n.GetKeyName())
	nodesMutex.Unlock()
}

func (s *ClusterMeshTestSuite) TestClusterMesh(c *C) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kvstore.SetupDummy("etcd")
	defer func() {
		kvstore.Client().DeletePrefix(context.TODO(), kvstore.BaseKeyPrefix)
		kvstore.Client().Close(ctx)
	}()

	identity.InitWellKnownIdentities(&fakeConfig.Config{})
	// The nils are only used by k8s CRD identities. We default to kvstore.
	mgr := cache.NewCachingIdentityAllocator(&testidentity.IdentityAllocatorOwnerMock{})
	<-mgr.InitIdentityAllocator(nil)
	defer mgr.Close()

	dir, err := os.MkdirTemp("", "multicluster")
	c.Assert(err, IsNil)
	defer os.RemoveAll(dir)

	etcdConfig := []byte(fmt.Sprintf("endpoints:\n- %s\n", kvstore.EtcdDummyAddress()))

	// cluster3 doesn't have cluster configuration on kvstore. This emulates
	// the old Cilium version which doesn't support cluster configuration
	// feature. We should be able to connect to such a cluster for
	// compatibility.
	for i, name := range []string{"test2", "cluster1", "cluster2"} {
		config := cmtypes.CiliumClusterConfig{
			ID: uint32(i + 1),
		}

		if name == "cluster2" {
			// Cluster2 supports synced canaries
			config.Capabilities.SyncedCanaries = true
		}

		err = cmutils.SetClusterConfig(ctx, name, &config, kvstore.Client())
		c.Assert(err, IsNil)
	}

	config1 := path.Join(dir, "cluster1")
	err = os.WriteFile(config1, etcdConfig, 0644)
	c.Assert(err, IsNil)

	config2 := path.Join(dir, "cluster2")
	err = os.WriteFile(config2, etcdConfig, 0644)
	c.Assert(err, IsNil)

	config3 := path.Join(dir, "cluster3")
	err = os.WriteFile(config3, etcdConfig, 0644)
	c.Assert(err, IsNil)

	ipc := ipcache.NewIPCache(&ipcache.Configuration{
		Context: ctx,
	})
	defer ipc.Shutdown()

	cm := NewClusterMesh(hivetest.Lifecycle(c), Configuration{
		Config:                internal.Config{ClusterMeshConfig: dir},
		ClusterIDName:         types.ClusterIDName{ClusterID: 255, ClusterName: "test2"},
		NodeKeyCreator:        testNodeCreator,
		NodeObserver:          &testObserver{},
		RemoteIdentityWatcher: mgr,
		IPCache:               ipc,
		Metrics:               newMetrics(),
		InternalMetrics:       internal.MetricsProvider(subsystem)(),
	})
	c.Assert(cm, Not(IsNil))

	// cluster2 is the cluster which is tested with sync canaries
	nodesWSS := store.NewWorkqueueSyncStore("cluster2", kvstore.Client(), nodeStore.NodeStorePrefix)
	go nodesWSS.Run(ctx)
	nodeNames := []string{"foo", "bar", "baz"}

	// wait for all clusters to appear in the list of cm clusters
	c.Assert(testutils.WaitUntil(func() bool {
		return cm.NumReadyClusters() == 3
	}, 10*time.Second), IsNil)

	// Ensure that ClusterIDs are reserved correctly after connect
	cm.usedIDs.usedClusterIDsMutex.Lock()
	_, ok := cm.usedIDs.usedClusterIDs[2]
	c.Assert(ok, Equals, true)
	_, ok = cm.usedIDs.usedClusterIDs[3]
	c.Assert(ok, Equals, true)
	// cluster3 doesn't have config, so only 2 IDs should be reserved
	c.Assert(cm.usedIDs.usedClusterIDs, HasLen, 2)
	cm.usedIDs.usedClusterIDsMutex.Unlock()

	// Reconnect cluster with changed ClusterID
	config := cmtypes.CiliumClusterConfig{
		ID: 255,
	}
	err = cmutils.SetClusterConfig(ctx, "cluster1", &config, kvstore.Client())
	c.Assert(err, IsNil)
	// Ugly hack to trigger config update
	etcdConfigNew := append(etcdConfig, []byte("\n")...)
	config1New := path.Join(dir, "cluster1")
	err = os.WriteFile(config1New, etcdConfigNew, 0644)
	c.Assert(err, IsNil)

	c.Assert(testutils.WaitUntil(func() bool {
		// Ensure if old ClusterID for cluster1 is released
		// and new ClusterID is reserved.
		cm.usedIDs.usedClusterIDsMutex.Lock()
		_, ok1 := cm.usedIDs.usedClusterIDs[2]
		_, ok2 := cm.usedIDs.usedClusterIDs[255]
		cm.usedIDs.usedClusterIDsMutex.Unlock()
		return ok1 == false && ok2 == true
	}, 10*time.Second), IsNil)

	for _, cluster := range []string{"cluster1", "cluster2", "cluster3"} {
		for _, name := range nodeNames {
			nodesWSS.UpsertKey(ctx, &testNode{Name: name, Cluster: cluster})
			c.Assert(err, IsNil)
		}
	}

	// Write the sync canary for cluster2
	nodesWSS.Synced(ctx)

	// wait for all cm nodes in both clusters to appear in the node list
	c.Assert(testutils.WaitUntil(func() bool {
		nodesMutex.RLock()
		defer nodesMutex.RUnlock()
		return len(nodes) == 3*len(nodeNames)
	}, 10*time.Second), IsNil)

	os.RemoveAll(config2)

	// wait for the removed cluster to disappear
	c.Assert(testutils.WaitUntil(func() bool {
		return cm.NumReadyClusters() == 2
	}, 5*time.Second), IsNil)

	// Make sure that ID is freed
	cm.usedIDs.usedClusterIDsMutex.Lock()
	_, ok = cm.usedIDs.usedClusterIDs[2]
	c.Assert(ok, Equals, false)
	c.Assert(cm.usedIDs.usedClusterIDs, HasLen, 1)
	cm.usedIDs.usedClusterIDsMutex.Unlock()

	// wait for the nodes of the removed cluster to disappear
	c.Assert(testutils.WaitUntil(func() bool {
		nodesMutex.RLock()
		defer nodesMutex.RUnlock()
		return len(nodes) == 2*len(nodeNames)
	}, 10*time.Second), IsNil)

	os.RemoveAll(config1)
	os.RemoveAll(config3)

	// wait for the removed cluster to disappear
	c.Assert(testutils.WaitUntil(func() bool {
		return cm.NumReadyClusters() == 0
	}, 5*time.Second), IsNil)

	// wait for the nodes of the removed cluster to disappear
	c.Assert(testutils.WaitUntil(func() bool {
		nodesMutex.RLock()
		defer nodesMutex.RUnlock()
		return len(nodes) == 0
	}, 10*time.Second), IsNil)

	// Make sure that IDs are freed
	cm.usedIDs.usedClusterIDsMutex.Lock()
	c.Assert(cm.usedIDs.usedClusterIDs, HasLen, 0)
	cm.usedIDs.usedClusterIDsMutex.Unlock()
}
