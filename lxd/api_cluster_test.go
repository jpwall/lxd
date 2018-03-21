package main

import (
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"testing"
	"time"

	lxd "github.com/lxc/lxd/client"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A LXD node which is already configured for networking can be converted to a
// single-node LXD cluster.
func TestCluster_Bootstrap(t *testing.T) {
	daemon, cleanup := newDaemon(t)
	defer cleanup()

	f := clusterFixture{t: t}
	f.EnableNetworking(daemon, "")

	client := f.ClientUnix(daemon)

	cluster := api.ClusterPut{}
	cluster.ServerName = "buzz"
	cluster.Enabled = true
	op, err := client.UpdateCluster(cluster, "")
	require.NoError(t, err)
	require.NoError(t, op.Wait())

	server, _, err := client.GetServer()
	require.NoError(t, err)
	assert.True(t, client.IsClustered())
	assert.Equal(t, "buzz", server.Environment.ServerName)
}

func TestCluster_Get(t *testing.T) {
	daemon, cleanup := newDaemon(t)
	defer cleanup()

	client, err := lxd.ConnectLXDUnix(daemon.UnixSocket(), nil)
	require.NoError(t, err)

	cluster, _, err := client.GetCluster()
	require.NoError(t, err)
	assert.Equal(t, "", cluster.ServerName)
	assert.False(t, cluster.Enabled)
}

// A LXD node which is already configured for networking can join an existing
// cluster.
func TestCluster_Join(t *testing.T) {
	daemons, cleanup := newDaemons(t, 2)
	defer cleanup()

	f := clusterFixture{t: t}
	passwords := []string{"sekret", ""}

	for i, daemon := range daemons {
		f.EnableNetworking(daemon, passwords[i])
	}

	// Bootstrap the cluster using the first node.
	client := f.ClientUnix(daemons[0])
	cluster := api.ClusterPut{}
	cluster.ServerName = "buzz"
	cluster.Enabled = true
	op, err := client.UpdateCluster(cluster, "")
	require.NoError(t, err)
	require.NoError(t, op.Wait())

	// Make the second node join the cluster.
	f.RegisterCertificate(daemons[1], daemons[0], "rusp", "sekret")
	address := daemons[0].endpoints.NetworkAddress()
	cert := string(daemons[0].endpoints.NetworkPublicKey())
	client = f.ClientUnix(daemons[1])
	cluster = api.ClusterPut{
		ClusterAddress:     address,
		ClusterCertificate: cert,
	}
	cluster.ServerName = "rusp"
	cluster.Enabled = true
	op, err = client.UpdateCluster(cluster, "")
	require.NoError(t, err)
	require.NoError(t, op.Wait())

	// At least the leader node is listed as database node in the second
	// node's sqlite database. Depending on the timing of the join request
	// and of the heartbeat update from the leader, there might be a second
	// entry for the joining node itself.
	state := daemons[1].State()
	err = state.Node.Transaction(func(tx *db.NodeTx) error {
		nodes, err := tx.RaftNodes()
		require.NoError(t, err)
		require.True(t, len(nodes) >= 1, "no rows in raft_nodes table")
		assert.Equal(t, int64(1), nodes[0].ID)
		assert.Equal(t, daemons[0].endpoints.NetworkAddress(), nodes[0].Address)

		if len(nodes) == 1 {
			return nil
		}
		require.Len(t, nodes, 2)
		assert.Equal(t, int64(2), nodes[1].ID)
		assert.Equal(t, daemons[1].endpoints.NetworkAddress(), nodes[1].Address)
		return nil
	})
	require.NoError(t, err)

	// Changing the configuration on the second node also updates it on the
	// first, via internal notifications.
	server, _, err := client.GetServer()
	require.NoError(t, err)
	serverPut := server.Writable()
	serverPut.Config["core.macaroon.endpoint"] = "foo.bar"
	require.NoError(t, client.UpdateServer(serverPut, ""))

	for _, daemon := range daemons {
		assert.NotNil(t, daemon.externalAuth)
	}

	// The GetClusterMembers client method returns both nodes.
	nodes, err := client.GetClusterMembers()
	require.NoError(t, err)
	assert.Len(t, nodes, 2)
	assert.Equal(t, "buzz", nodes[0].ServerName)
	assert.Equal(t, "rusp", nodes[1].ServerName)
	assert.Equal(t, "Online", nodes[0].Status)
	assert.Equal(t, "Online", nodes[1].Status)

	// The GetClusterMemberNames client method returns the URLs of both
	// nodes.
	urls, err := client.GetClusterMemberNames()
	require.NoError(t, err)
	assert.Len(t, urls, 2)
	assert.Equal(t, "/1.0/cluster/members/buzz", urls[0])
	assert.Equal(t, "/1.0/cluster/members/rusp", urls[1])

	// The GetNode method returns the requested node.
	node, _, err := client.GetClusterMember("buzz")
	require.NoError(t, err)
	assert.Equal(t, "buzz", node.ServerName)
}

// If the joining node hasn't added its certificate as trusted client
// certificate, an authorization error is returned.
func TestCluster_JoinUnauthorized(t *testing.T) {
	daemons, cleanup := newDaemons(t, 2)
	defer cleanup()

	f := clusterFixture{t: t}
	passwords := []string{"sekret", ""}

	for i, daemon := range daemons {
		f.EnableNetworking(daemon, passwords[i])
	}

	// Bootstrap the cluster using the first node.
	client := f.ClientUnix(daemons[0])
	cluster := api.ClusterPut{}
	cluster.ServerName = "buzz"
	cluster.Enabled = true
	op, err := client.UpdateCluster(cluster, "")
	require.NoError(t, err)
	require.NoError(t, op.Wait())

	// Make the second node join the cluster.
	address := daemons[0].endpoints.NetworkAddress()
	cert := string(daemons[0].endpoints.NetworkPublicKey())
	client = f.ClientUnix(daemons[1])
	cluster = api.ClusterPut{
		ClusterAddress:     address,
		ClusterCertificate: cert,
	}
	cluster.ServerName = "rusp"
	cluster.Enabled = true
	op, err = client.UpdateCluster(cluster, "")
	require.NoError(t, err)
	assert.EqualError(t, op.Wait(), "failed to request to add node: not authorized")
}

// In a cluster for 3 nodes, if the leader goes down another one is elected the
// other two nodes continue to operate fine.
func DISABLED_TestCluster_Failover(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cluster failover test in short mode.")
	}
	daemons, cleanup := newDaemons(t, 3)
	defer cleanup()

	f := clusterFixture{t: t}
	f.FormCluster(daemons)

	require.NoError(t, daemons[0].Stop())

	for i, daemon := range daemons[1:] {
		t.Logf("Invoking GetServer API against daemon %d", i)
		client := f.ClientUnix(daemon)
		server, _, err := client.GetServer()
		require.NoError(f.t, err)
		serverPut := server.Writable()
		serverPut.Config["core.trust_password"] = fmt.Sprintf("sekret-%d", i)

		t.Logf("Invoking UpdateServer API against daemon %d", i)
		require.NoError(f.t, client.UpdateServer(serverPut, ""))
	}
}

// A node can leave a cluster gracefully.
func TestCluster_Leave(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cluster leave test in short mode.")
	}
	daemons, cleanup := newDaemons(t, 2)
	defer cleanup()

	f := clusterFixture{t: t}
	f.FormCluster(daemons)

	client := f.ClientUnix(daemons[1])
	err := client.DeleteClusterMember("rusp-0", false)
	require.NoError(t, err)

	_, _, err = client.GetServer()
	require.NoError(t, err)
	assert.False(t, client.IsClustered())

	nodes, err := client.GetClusterMembers()
	require.NoError(t, err)
	assert.Len(t, nodes, 1)
	assert.Equal(t, "none", nodes[0].ServerName)
}

// A node can't leave a cluster gracefully if it still has images associated
// with it.
func TestCluster_LeaveWithImages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cluster leave test in short mode.")
	}
	daemons, cleanup := newDaemons(t, 2)
	defer cleanup()

	f := clusterFixture{t: t}
	f.FormCluster(daemons)

	daemon := daemons[1]
	err := daemon.State().Cluster.ImageInsert(
		"abc", "foo", 123, false, false, "amd64", time.Now(), time.Now(), nil)
	require.NoError(t, err)

	client := f.ClientUnix(daemons[1])
	err = client.DeleteClusterMember("rusp-0", false)
	assert.EqualError(t, err, "node still has the following images: abc")

	// If we now associate the image with the other node as well, leaving
	// the cluster is fine.
	daemon = daemons[0]
	err = daemon.State().Cluster.ImageAssociateNode("abc")
	require.NoError(t, err)

	err = client.DeleteClusterMember("rusp-0", false)
	assert.NoError(t, err)
}

// The force flag makes a node leave also if it still has images.
func TestCluster_LeaveForce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cluster leave test in short mode.")
	}
	daemons, cleanup := newDaemons(t, 2)
	defer cleanup()

	f := clusterFixture{t: t}
	f.FormCluster(daemons)

	daemon := daemons[1]
	err := daemon.State().Cluster.ImageInsert(
		"abc", "foo", 123, false, false, "amd64", time.Now(), time.Now(), nil)
	require.NoError(t, err)

	client := f.ClientUnix(daemons[1])
	err = client.DeleteClusterMember("rusp-0", true)
	assert.NoError(t, err)

	// The image is gone, since the deleted node was the only one having a
	// copy of it.
	daemon = daemons[0]
	images, err := daemon.State().Cluster.ImagesGet(false)
	require.NoError(t, err)
	assert.Equal(t, []string{}, images)
}

// A LXD node can be renamed.
func TestCluster_NodeRename(t *testing.T) {
	daemon, cleanup := newDaemon(t)
	defer cleanup()

	f := clusterFixture{t: t}
	f.EnableNetworking(daemon, "")

	client := f.ClientUnix(daemon)

	cluster := api.ClusterPut{}
	cluster.ServerName = "buzz"
	cluster.Enabled = true
	op, err := client.UpdateCluster(cluster, "")
	require.NoError(t, err)
	require.NoError(t, op.Wait())

	node := api.ClusterMemberPost{ServerName: "rusp"}
	err = client.RenameClusterMember("buzz", node)
	require.NoError(t, err)

	_, _, err = client.GetClusterMember("rusp")
	require.NoError(t, err)
}

// Test helper for cluster-related APIs.
type clusterFixture struct {
	t       *testing.T
	clients map[*Daemon]lxd.ContainerServer
}

// Form a cluster using the given daemons. The first daemon will be the leader.
func (f *clusterFixture) FormCluster(daemons []*Daemon) {
	for i, daemon := range daemons {
		password := ""
		if i == 0 {
			password = "sekret"
		}
		f.EnableNetworking(daemon, password)
	}

	// Bootstrap the cluster using the first node.
	client := f.ClientUnix(daemons[0])
	cluster := api.ClusterPut{}
	cluster.ServerName = "buzz"
	cluster.Enabled = true
	op, err := client.UpdateCluster(cluster, "")
	require.NoError(f.t, err)
	require.NoError(f.t, op.Wait())

	// Make the other nodes join the cluster.
	address := daemons[0].endpoints.NetworkAddress()
	cert := string(daemons[0].endpoints.NetworkPublicKey())
	for i, daemon := range daemons[1:] {
		name := fmt.Sprintf("rusp-%d", i)
		f.RegisterCertificate(daemon, daemons[0], name, "sekret")
		client = f.ClientUnix(daemon)
		cluster := api.ClusterPut{
			ClusterAddress:     address,
			ClusterCertificate: cert,
		}
		cluster.ServerName = name
		cluster.Enabled = true
		op, err = client.UpdateCluster(cluster, "")
		require.NoError(f.t, err)
		require.NoError(f.t, op.Wait())
	}
}

// Enable networking in the given daemon. The password is optional and can be
// an empty string.
func (f *clusterFixture) EnableNetworking(daemon *Daemon, password string) {
	port, err := shared.AllocatePort()
	require.NoError(f.t, err)

	address := fmt.Sprintf("127.0.0.1:%d", port)

	client := f.ClientUnix(daemon)
	server, _, err := client.GetServer()
	require.NoError(f.t, err)
	serverPut := server.Writable()
	serverPut.Config["core.https_address"] = address
	serverPut.Config["core.trust_password"] = password

	require.NoError(f.t, client.UpdateServer(serverPut, ""))
}

// Register daemon1's server certificate as daemon2's trusted certificate,
// using password authentication.
func (f *clusterFixture) RegisterCertificate(daemon1, daemon2 *Daemon, name, password string) {
	client1 := f.ClientUnix(daemon1)
	server, _, err := client1.GetServer()
	require.NoError(f.t, err)
	block, _ := pem.Decode([]byte(server.Environment.Certificate))
	require.NotNil(f.t, block)
	certificate := base64.StdEncoding.EncodeToString(block.Bytes)
	post := api.CertificatesPost{
		Password:    password,
		Certificate: certificate,
	}
	post.Name = fmt.Sprintf("lxd.cluster.%s", name)
	post.Type = "client"

	client2 := f.ClientUnix(daemon2)
	err = client2.CreateCertificate(post)
	require.NoError(f.t, err)
}

// Get a client for the given daemon connected via UNIX socket, creating one if
// needed.
func (f *clusterFixture) ClientUnix(daemon *Daemon) lxd.ContainerServer {
	if f.clients == nil {
		f.clients = make(map[*Daemon]lxd.ContainerServer)
	}
	client, ok := f.clients[daemon]
	if !ok {
		var err error
		client, err = lxd.ConnectLXDUnix(daemon.UnixSocket(), nil)
		require.NoError(f.t, err)
	}
	return client
}