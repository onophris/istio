// Copyright 2018 Istio Authors
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

package v2

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	xdsapi "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	"github.com/gogo/protobuf/types"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/log"
)

// EDS returns the list of endpoints (IP:port and in future labels) associated with a real
// service or a subset of a service, selected using labels.
//
// The source of info is a list of service registries.
//
// Primary event is an endpoint creation/deletion. Once the event is fired, EDS needs to
// find the list of services associated with the endpoint.
//
// In case of k8s, Endpoints event is fired when the endpoints are added to service - typically
// after readiness check. At that point we have the 'real' Service. The Endpoint includes a list
// of port numbers and names.
//
// For the subset case, the Pod referenced in the Endpoint must be looked up, and pod checked
// for labels.
//
// In addition, ExternalEndpoint includes IPs and labels directly and can be directly processed.
//
// TODO: for selector-less services (mesh expansion), skip pod processing
// TODO: optimize the code path for ExternalEndpoint, no additional processing needed
// TODO: if a service doesn't have split traffic - we can also skip pod and lable processing
// TODO: efficient label processing. In alpha3, the destination policies are set per service, so
// we may only need to search in a small list.

var (
	// It doesn't appear the logging framework has support for per-component logging, yet
	// we need to be able to debug EDS2.
	// Default is enabled (not set: "" != "0")
	edsDebug = os.Getenv("PILOT_DEBUG_EDS") != "0"

	edsClusterMutex sync.Mutex
	edsClusters     = map[string]*EdsCluster{}

	// Tracks connections, increment on each new connection.
	connectionNumber = int64(0)
)

// EdsCluster tracks eds-related info for monitored clusters. In practice it'll include
// all clusters until we support on-demand cluster loading.
type EdsCluster struct {
	// mutex protects changes to this cluster
	mutex sync.Mutex

	LoadAssignment *xdsapi.ClusterLoadAssignment

	// FirstUse is the time the cluster was first used, for debugging
	FirstUse time.Time

	// EdsClients keeps track of all nodes monitoring the cluster.
	EdsClients map[string]*XdsConnection `json:"-"`

	// NonEmptyTime is the time the cluster first had a non-empty set of endpoints
	NonEmptyTime time.Time

	// The discovery service this cluster is associated with.
	discovery *DiscoveryServer
}

// TODO: add prom metrics !

// Endpoints aggregate a DiscoveryResponse for pushing.
func (s *DiscoveryServer) endpoints(clusterNames []string, outRes []types.Any) *xdsapi.DiscoveryResponse {
	out := &xdsapi.DiscoveryResponse{
		// All resources for EDS ought to be of the type ClusterLoadAssignment
		TypeUrl: EndpointType,

		// Pilot does not really care for versioning. It always supplies what's currently
		// available to it, irrespective of whether Envoy chooses to accept or reject EDS
		// responses. Pilot believes in eventual consistency and that at some point, Envoy
		// will begin seeing results it deems to be good.
		VersionInfo: versionInfo(),
		Nonce:       nonce(),
		Resources:   outRes,
	}

	return out
}

// Return the load assignment. The field can be updated by another routine.
func loadAssignment(c *EdsCluster) *xdsapi.ClusterLoadAssignment {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.LoadAssignment
}

func newEndpoint(address string, port uint32) (*endpoint.LbEndpoint, error) {
	ipAddr := net.ParseIP(address)
	if ipAddr == nil {
		return nil, errors.New("Invalid IP address " + address)
	}
	ep := &endpoint.LbEndpoint{
		Endpoint: &endpoint.Endpoint{
			Address: &core.Address{
				Address: &core.Address_SocketAddress{
					&core.SocketAddress{
						Address:    address,
						Ipv4Compat: ipAddr.To4() != nil,
						Protocol:   core.TCP,
						PortSpecifier: &core.SocketAddress_PortValue{
							PortValue: port,
						},
					},
				},
			},
		},
	}

	//log.Infoa("EDS: endpoint ", ipAddr, ep.String())
	return ep, nil
}

// updateCluster is called from the event (or global cache invalidation) to update
// the endpoints for the cluster.
func updateCluster(clusterName string, edsCluster *EdsCluster) error {
	// TODO: should we lock this as well ? Once we move to event-based it may not matter.
	var hostname string
	var ports model.PortList
	var labels model.LabelsCollection
	// Single port
	var portName string

	// This is a gross hack but Costin will insist on supporting everything from ancient Greece
	if strings.Index(clusterName, "outbound") == 0 ||
		strings.Index(clusterName, "inbound") == 0 { //new style cluster names
		var p *model.Port
		var subsetName string
		_, subsetName, hostname, p = model.ParseSubsetKey(clusterName)
		ports = []*model.Port{p}
		portName = p.Name
		labels = edsCluster.discovery.env.IstioConfigStore.SubsetToLabels(subsetName, hostname)
	} else {
		hostname, ports, labels = model.ParseServiceKey(clusterName)
		if len(ports) > 0 {
			portName = ports.GetNames()[0]
		}
	}

	instances, err := edsCluster.discovery.env.ServiceDiscovery.Instances(hostname, ports.GetNames(), labels)
	if err != nil {
		log.Warnf("endpoints for service cluster %q returned error %q", clusterName, err)
		return err
	}
	locEps := localityLbEndpointsFromInstances(instances)
	if len(instances) == 0 {
		log.Warnf("EDS: no instances %s (host=%s ports=%v labels=%v)", clusterName, hostname, portName, labels)
	}
	edsInstances.With(prometheus.Labels{"cluster": clusterName}).Set(float64(len(instances)))
	// There is a chance multiple goroutines will update the cluster at the same time.
	// This could be prevented by a lock - but because the update may be slow, it may be
	// better to accept the extra computations.
	// We still lock the access to the LoadAssignments.
	edsCluster.mutex.Lock()
	defer edsCluster.mutex.Unlock()
	edsCluster.LoadAssignment = &xdsapi.ClusterLoadAssignment{
		ClusterName: clusterName,
		Endpoints:   locEps,
	}
	if len(locEps) > 0 && edsCluster.NonEmptyTime.IsZero() {
		edsCluster.NonEmptyTime = time.Now()
	}
	return nil
}

// LocalityLbEndpointsFromInstances returns a list of Envoy v2 LocalityLbEndpoints.
// Envoy v2 Endpoints are constructed from Pilot's older data structure involving
// model.ServiceInstance objects. Envoy expects the endpoints grouped by zone, so
// a map is created - in new data structures this should be part of the model.
func localityLbEndpointsFromInstances(instances []*model.ServiceInstance) []endpoint.LocalityLbEndpoints {
	localityEpMap := make(map[string]*endpoint.LocalityLbEndpoints)
	for _, instance := range instances {
		lbEp, err := newEndpoint(instance.Endpoint.Address, (uint32)(instance.Endpoint.Port))
		if err != nil {
			log.Errorf("EDS: unexpected pilot model endpoint v1 to v2 conversion: %v", err)
			continue
		}
		// TODO: Need to accommodate region, zone and subzone. Older Pilot datamodel only has zone = availability zone.
		// Once we do that, the key must be a | separated tupple.
		locality := instance.AvailabilityZone
		locLbEps, found := localityEpMap[locality]
		if !found {
			locLbEps = &endpoint.LocalityLbEndpoints{
				Locality: &core.Locality{
					Zone: instance.AvailabilityZone,
				},
			}
			localityEpMap[locality] = locLbEps
		}
		locLbEps.LbEndpoints = append(locLbEps.LbEndpoints, *lbEp)
	}
	out := make([]endpoint.LocalityLbEndpoints, 0, len(localityEpMap))
	for _, locLbEps := range localityEpMap {
		out = append(out, *locLbEps)
	}
	return out
}

func connectionID(node string) string {
	edsClusterMutex.Lock()
	connectionNumber++
	c := connectionNumber
	edsClusterMutex.Unlock()
	return node + "-" + strconv.Itoa(int(c))
}

// StreamEndpoints implements xdsapi.EndpointDiscoveryServiceServer.StreamEndpoints().
func (s *DiscoveryServer) StreamEndpoints(stream xdsapi.EndpointDiscoveryService_StreamEndpointsServer) error {
	peerInfo, ok := peer.FromContext(stream.Context())
	peerAddr := "Unknown peer address"
	if ok {
		peerAddr = peerInfo.Addr.String()
	}
	var discReq *xdsapi.DiscoveryRequest
	var receiveError error
	reqChannel := make(chan *xdsapi.DiscoveryRequest, 1)

	initialRequestReceived := false

	con := &XdsConnection{
		pushChannel: make(chan *XdsEvent, 1),
		PeerAddr:    peerAddr,
		Clusters:    []string{},
		Connect:     time.Now(),
		stream:      stream,
	}
	// node is the key used in the cluster map. It includes the pod name and an unique identifier,
	// since multiple envoys may connect from the same pod.
	var conID string
	go func() {
		defer close(reqChannel)
		for {
			req, err := stream.Recv()
			if err != nil {
				log.Errorf("EDS: close for client %s %q terminated with errors %v",
					conID, peerAddr, err)
				if status.Code(err) == codes.Canceled || err == io.EOF {
					return
				}
				receiveError = err
				return
			}
			reqChannel <- req
		}
	}()

	for {
		// Block until either a request is received or the ticker ticks
		select {
		case discReq, ok = <-reqChannel:
			if !ok {
				return receiveError
			}

			// Should not change. A node monitors multiple clusters
			if conID == "" && discReq.Node != nil {
				conID = connectionID(discReq.Node.Id)
			}

			clusters2 := discReq.GetResourceNames()
			if initialRequestReceived {
				if len(clusters2) > len(con.Clusters) {
					// This doesn't happen with current envoy - but should happen in future, there is no reason
					// to keep so many open grpc streams (one per cluster, for each envoy)
					log.Infof("EDS: Multiple clusters monitoring %v -> %v %s", con.Clusters, clusters2, discReq.String())
					initialRequestReceived = false // treat this as an initial request (updates monitoring state)
				}
			}

			// Given that Pilot holds an eventually consistent data model, Pilot ignores any acknowledgements
			// from Envoy, whether they indicate ack success or ack failure of Pilot's previous responses.
			if initialRequestReceived {
				// TODO: once the deps are updated, log the ErrorCode if set (missing in current version)
				if discReq.ErrorDetail != nil {
					log.Warnf("EDS: ACK ERROR %v %s %v", peerAddr, conID, discReq.String())
				}
				if edsDebug {
					log.Infof("EDS: ACK %s %s %s", conID, discReq.VersionInfo, con.Clusters)
				}
				if len(con.Clusters) > 0 {
					continue
				}
			}
			if edsDebug {
				log.Infof("EDS: REQ %s %v %v raw: %s ", conID, con.Clusters, peerAddr, discReq.String())
			}
			con.Clusters = discReq.GetResourceNames()
			con.ConID = conID
			initialRequestReceived = true

			// In 0.7 EDS only listens for 1 cluster for each stream. In 0.8 EDS is no longer
			// used.
			for _, c := range con.Clusters {
				s.addEdsCon(c, conID, con)
			}

			// Keep track of active EDS client. In 0.7 EDS push happened by pushing for all
			// tracked clusters. In 0.8+ push happens by iterating active connections, in ADS.
			if !con.added {
				con.added = true
				s.addCon(conID, con)
				defer s.removeCon(conID, con)
			}

		case <-con.pushChannel:
		}

		if len(con.Clusters) > 0 {
			err := s.pushEds(con)
			if err != nil {
				return err
			}
		}

	}
}

func (s *DiscoveryServer) pushEds(con *XdsConnection) error {
	resAny := []types.Any{}

	emptyClusters := 0
	endpoints := 0
	empty := []string{}

	for _, clusterName := range con.Clusters {
		c := s.getOrAddEdsCluster(clusterName)
		l := loadAssignment(c)
		if l == nil { // fresh cluster
			if err := updateCluster(clusterName, c); err != nil {
				log.Errorf("error returned from updateCluster for cluster name %s, skipping it.", clusterName)
				continue
			}
			l = loadAssignment(c)
		}
		endpoints += len(l.Endpoints)
		if len(l.Endpoints) == 0 {
			emptyClusters++
			empty = append(empty, clusterName)
		}

		// Previously computed load assignments. They are re-computed on cache invalidation or
		// event, but don't have to be recomputed once for each sidecar.
		clAssignmentRes, _ := types.MarshalAny(l)
		resAny = append(resAny, *clAssignmentRes)
	}

	response := s.endpoints(con.Clusters, resAny)
	err := con.stream.Send(response)
	if err != nil {
		log.Warnf("EDS: Send failure, closing grpc %v", err)
		return err
	}

	if edsDebug {
		log.Infof("EDS: PUSH for %s %q clusters %d endpoints %d empty %d %v",
			con.ConID, con.PeerAddr, len(con.Clusters), endpoints, emptyClusters, empty)
	}
	return nil
}

func edsPushAll() {
	edsClusterMutex.Lock()
	// Create a temp map to avoid locking the add/remove
	tmpMap := map[string]*EdsCluster{}
	for k, v := range edsClusters {
		tmpMap[k] = v
	}
	edsClusterMutex.Unlock()

	for clusterName, edsCluster := range tmpMap {
		if err := updateCluster(clusterName, edsCluster); err != nil {
			log.Errorf("updateCluster failed with clusterName %s", clusterName)
			continue
		}
		edsCluster.mutex.Lock()
		for _, edsCon := range edsCluster.EdsClients {
			edsCon.pushChannel <- &XdsEvent{
				clusters: []string{clusterName},
			}
		}
		edsCluster.mutex.Unlock()
	}
}

// addEdsCon will track the eds connection with clusters, for optimized event-based push and debug
func (s *DiscoveryServer) addEdsCon(clusterName string, node string, connection *XdsConnection) {

	c := s.getOrAddEdsCluster(clusterName)
	// TODO: left the code here so we can skip sending the already-sent clusters.
	// See comments in ads - envoy keeps adding one cluster to the list (this seems new
	// previous version sent all the clusters from CDS in bulk).

	//c.mutex.Lock()
	//existing := c.EdsClients[node]
	//c.mutex.Unlock()
	//
	//// May replace an existing connection: this happens when Envoy adds more clusters
	//// one by one, creating new grpc requests each time it adds one more cluster.
	//if existing != nil {
	//	log.Warnf("Replacing existing connection %s %s old: %s", clusterName, node, existing.ConID)
	//}
	c.mutex.Lock()
	c.EdsClients[node] = connection
	c.mutex.Unlock()
}

// getEdsCluster returns a cluster.
func (s *DiscoveryServer) getEdsCluster(clusterName string) *EdsCluster {
	// separate method only to have proper lock.
	edsClusterMutex.Lock()
	defer edsClusterMutex.Unlock()
	return edsClusters[clusterName]
}

func (s *DiscoveryServer) getOrAddEdsCluster(clusterName string) *EdsCluster {
	edsClusterMutex.Lock()
	defer edsClusterMutex.Unlock()

	c := edsClusters[clusterName]
	if c == nil {
		c = &EdsCluster{discovery: s,
			EdsClients: map[string]*XdsConnection{},
			FirstUse:   time.Now(),
		}
		edsClusters[clusterName] = c
	}
	return c
}

// removeEdsCon is called when a gRPC stream is closed, for each cluster that was watched by the
// stream. As of 0.7 envoy watches a single cluster per gprc stream.
func (s *DiscoveryServer) removeEdsCon(clusterName string, node string, connection *XdsConnection) {
	c := s.getEdsCluster(clusterName)
	if c == nil {
		log.Warnf("EDS: missing cluster %s", clusterName)
		return
	}

	c.mutex.Lock()
	defer c.mutex.Unlock()

	oldcon := c.EdsClients[node]
	if oldcon == nil {
		log.Warnf("EDS: Envoy restart %s %v, cleanup old connection missing %v", node, connection.PeerAddr, c.EdsClients)
		return
	}
	if oldcon != connection {
		if edsDebug {
			log.Infof("EDS: Envoy restart %s %v, cleanup old connection %v", node, connection.PeerAddr, oldcon.PeerAddr)
		}
		return
	}
	delete(c.EdsClients, node)
	if len(c.EdsClients) == 0 {
		edsClusterMutex.Lock()
		defer edsClusterMutex.Unlock()
		log.Infof("EDS: remove unused cluster node=%s cluster=%s all=%v", node, clusterName, edsClusters)
		delete(edsClusters, clusterName)
	}
}

// FetchEndpoints implements xdsapi.EndpointDiscoveryServiceServer.FetchEndpoints().
func (s *DiscoveryServer) FetchEndpoints(ctx context.Context, req *xdsapi.DiscoveryRequest) (*xdsapi.DiscoveryResponse, error) {
	return nil, errors.New("not implemented")
}

// StreamLoadStats implements xdsapi.EndpointDiscoveryServiceServer.StreamLoadStats().
func (s *DiscoveryServer) StreamLoadStats(xdsapi.EndpointDiscoveryService_StreamEndpointsServer) error {
	return errors.New("unsupported streaming method")
}
