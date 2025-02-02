/***
Copyright 2014 Cisco Systems Inc. All rights reserved.

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
package ofnet

// This file implements the vxlan bridging datapath

import (
	//"fmt"
	"errors"
	"net"
	"net/rpc"
	"strings"

	"github.com/contiv/ofnet/ofctrl"

	log "github.com/Sirupsen/logrus"
)

// VXLAN tables are structured as follows
//
// +-------+
// | Valid |
// | Pkts  +-->+-------+
// +-------+   | Vlan  |
//             | Table +-------+          +---------+
//             +-------+       +--------->| Mac Dst |      +--------------+
//                                        | Lookup  +--+-->| Ucast Output |
//                                        +---------+  |   +--------------+
//                                                     |
//                                                     |
//                                     +---------------+----------+
//                                     V                          V
//                            +------------------+    +----------------------+
//                            | Local Only Flood |    | Local + Remote Flood |
//                            +------------------+    +----------------------+
//

// Vxlan state.
type Vxlan struct {
	agent       *OfnetAgent      // Pointer back to ofnet agent that owns this
	ofSwitch    *ofctrl.OFSwitch // openflow switch we are talking to
	policyAgent *PolicyAgent     // Policy agent

	vlanDb map[uint16]*Vlan // Database of known vlans

	// Fgraph tables
	inputTable   *ofctrl.Table // Packet lookup starts here
	vlanTable    *ofctrl.Table // Vlan Table. map port or VNI to vlan
	macDestTable *ofctrl.Table // Destination mac lookup

	// Flow Database
	macFlowDb      map[string]*ofctrl.Flow // Database of flow entries
	portVlanFlowDb map[uint32]*ofctrl.Flow // Database of flow entries
}

// Vlan info
type Vlan struct {
	Vni            uint32                  // Vxlan VNI
	localPortList  map[uint32]*uint32      // List of local ports only
	allPortList    map[uint32]*uint32      // List of local + remote(vtep) ports
	vtepVlanFlowDb map[uint32]*ofctrl.Flow // VTEP vlan mapping flows
	localFlood     *ofctrl.Flood           // local only flood list
	allFlood       *ofctrl.Flood           // local + remote flood list
	localMacMiss   *ofctrl.Flow            // mac lookup miss entry for locally originated traffic
	vtepMacMiss    *ofctrl.Flow            // mac lookup miss for traffic coming from vtep
}

const METADATA_RX_VTEP = 0x1

// Create a new vxlan instance
func NewVxlan(agent *OfnetAgent, rpcServ *rpc.Server) *Vxlan {
	vxlan := new(Vxlan)

	// Keep a reference to the agent
	vxlan.agent = agent

	// Create policy agent
	vxlan.policyAgent = NewPolicyAgent(agent, rpcServ)

	// init DBs
	vxlan.vlanDb = make(map[uint16]*Vlan)
	vxlan.macFlowDb = make(map[string]*ofctrl.Flow)
	vxlan.portVlanFlowDb = make(map[uint32]*ofctrl.Flow)

	return vxlan
}

// Handle new master added event
func (self *Vxlan) MasterAdded(master *OfnetNode) error {

	return nil
}

// Handle switch connected notification
func (self *Vxlan) SwitchConnected(sw *ofctrl.OFSwitch) {
	// Keep a reference to the switch
	self.ofSwitch = sw

	// Tell the policy agent about the switch
	self.policyAgent.SwitchConnected(sw)

	// Init the Fgraph
	self.initFgraph()

	log.Infof("Switch connected(vxlan)")
}

// Handle switch disconnected notification
func (self *Vxlan) SwitchDisconnected(sw *ofctrl.OFSwitch) {
	// FIXME: ??
}

// Handle incoming packet
func (self *Vxlan) PacketRcvd(sw *ofctrl.OFSwitch, pkt *ofctrl.PacketIn) {
	// Ignore all incoming packets for now
}

// Add a local endpoint and install associated local route
func (self *Vxlan) AddLocalEndpoint(endpoint OfnetEndpoint) error {
	log.Infof("Adding localEndpoint: %+v", endpoint)

	vni := self.agent.vlanVniMap[endpoint.Vlan]
	if vni == nil {
		log.Errorf("VNI for vlan %d is not known", endpoint.Vlan)
		return errors.New("Unknown Vlan")
	}

	// Install a flow entry for vlan mapping and point it to Mac table
	portVlanFlow, err := self.vlanTable.NewFlow(ofctrl.FlowMatch{
		Priority:  FLOW_MATCH_PRIORITY,
		InputPort: endpoint.PortNo,
	})
	if err != nil {
		log.Errorf("Error creating portvlan entry. Err: %v", err)
		return err
	}

	// Set source endpoint group if specified
	if endpoint.EndpointGroup != 0 {
		metadata, metadataMask := SrcGroupMetadata(endpoint.EndpointGroup)
		portVlanFlow.SetMetadata(metadata, metadataMask)
	}

	// Set the vlan and install it
	portVlanFlow.SetVlan(endpoint.Vlan)
	dstGrpTbl := self.ofSwitch.GetTable(DST_GRP_TBL_ID)
	err = portVlanFlow.Next(dstGrpTbl)
	if err != nil {
		log.Errorf("Error installing portvlan entry. Err: %v", err)
		return err
	}

	// save the flow entry
	self.portVlanFlowDb[endpoint.PortNo] = portVlanFlow

	// Add the port to local and remote flood list
	output, err := self.ofSwitch.OutputPort(endpoint.PortNo)
	if err != nil {
		return err
	}
	vlan := self.vlanDb[endpoint.Vlan]
	if vlan != nil {
		vlan.localFlood.AddOutput(output)
		vlan.allFlood.AddOutput(output)
	}

	macAddr, err := net.ParseMAC(endpoint.MacAddrStr)
	if err != nil {
		return err
	}

	// Finally install the mac address
	macFlow, err := self.macDestTable.NewFlow(ofctrl.FlowMatch{
		Priority: FLOW_MATCH_PRIORITY,
		VlanId:   endpoint.Vlan,
		MacDa:    &macAddr,
	})
	if err != nil {
		log.Errorf("Error creating mac flow for endpoint %+v. Err: %v", endpoint, err)
		return err
	}

	// Remove vlan tag and point it to local port
	macFlow.PopVlan()
	macFlow.Next(output)

	// Install dst group entry for the endpoint
	err = self.policyAgent.AddEndpoint(&endpoint)
	if err != nil {
		log.Errorf("Error adding endpoint to policy agent{%+v}. Err: %v", endpoint, err)
		return err
	}

	// Save the flow in DB
	self.macFlowDb[endpoint.MacAddrStr] = macFlow

	return nil
}

// Remove local endpoint
func (self *Vxlan) RemoveLocalEndpoint(endpoint OfnetEndpoint) error {
	// Remove the port from flood lists
	vlanId := self.agent.vniVlanMap[endpoint.Vni]
	vlan := self.vlanDb[*vlanId]
	output, err := self.ofSwitch.OutputPort(endpoint.PortNo)
	if err == nil {
		vlan.localFlood.RemoveOutput(output)
		vlan.allFlood.RemoveOutput(output)
	}

	// Remove the port vlan flow.
	portVlanFlow := self.portVlanFlowDb[endpoint.PortNo]
	if portVlanFlow != nil {
		err := portVlanFlow.Delete()
		if err != nil {
			log.Errorf("Error deleting portvlan flow. Err: %v", err)
		}
	}

	// find the flow
	macFlow := self.macFlowDb[endpoint.MacAddrStr]
	if macFlow == nil {
		log.Errorf("Could not find the flow for endpoint: %+v", endpoint)
		return errors.New("Mac flow not found")
	}

	// Delete the flow
	err = macFlow.Delete()
	if err != nil {
		log.Errorf("Error deleting mac flow: %+v. Err: %v", macFlow, err)
	}

	// Remove the endpoint from policy tables
	err = self.policyAgent.DelEndpoint(&endpoint)
	if err != nil {
		log.Errorf("Error deleting endpoint to policy agent{%+v}. Err: %v", endpoint, err)
		return err
	}

	return nil
}

// Add virtual tunnel end point. This is mainly used for mapping remote vtep IP
// to ofp port number.
func (self *Vxlan) AddVtepPort(portNo uint32, remoteIp net.IP) error {
	// Install VNI to vlan mapping for each vni
	for vni, vlan := range self.agent.vniVlanMap {
		// Install a flow entry for  VNI/vlan and point it to macDest table
		portVlanFlow, err := self.vlanTable.NewFlow(ofctrl.FlowMatch{
			Priority:  FLOW_MATCH_PRIORITY,
			InputPort: portNo,
			TunnelId:  uint64(vni),
		})
		if err != nil && strings.Contains(err.Error(), "Flow already exists") {
			log.Infof("VTEP %s already exists", remoteIp.String())
			return nil
		} else if err != nil {
			log.Errorf("Error adding Flow for VNI %d. Err: %v", vni, err)
			return err
		}
		portVlanFlow.SetVlan(*vlan)

		// Set the metadata to indicate packet came in from VTEP port
		portVlanFlow.SetMetadata(METADATA_RX_VTEP, METADATA_RX_VTEP)

		// Point to next table
		// Note that we bypass policy lookup on dest host.
		portVlanFlow.Next(self.macDestTable)

		// save the port vlan flow for cleaning up later
		self.vlanDb[*vlan].vtepVlanFlowDb[portNo] = portVlanFlow
	}

	// Walk all vlans and add vtep port to the vlan
	for vlanId, vlan := range self.vlanDb {
		vni := self.agent.vlanVniMap[vlanId]
		if vni == nil {
			log.Errorf("Can not find vni for vlan: %d", vlanId)
		}
		output, err := self.ofSwitch.OutputPort(portNo)
		if err != nil {
			return err
		}
		vlan.allFlood.AddTunnelOutput(output, uint64(*vni))
	}

	return nil
}

// Remove a VTEP port
func (self *Vxlan) RemoveVtepPort(portNo uint32, remoteIp net.IP) error {
	// Remove the VTEP from flood lists
	output, _ := self.ofSwitch.OutputPort(portNo)
	for _, vlan := range self.vlanDb {
		// Walk all vlans and remove from flood lists
		vlan.allFlood.RemoveOutput(output)
	}

	// FIXME: uninstall vlan-vni mapping.

	return nil
}

// Add a vlan.
func (self *Vxlan) AddVlan(vlanId uint16, vni uint32) error {
	var err error
	// check if the vlan already exists. if it does, we are done
	if self.vlanDb[vlanId] != nil {
		return nil
	}

	// create new vlan object
	vlan := new(Vlan)
	vlan.Vni = vni
	vlan.localPortList = make(map[uint32]*uint32)
	vlan.allPortList = make(map[uint32]*uint32)
	vlan.vtepVlanFlowDb = make(map[uint32]*ofctrl.Flow)

	// Create flood entries
	vlan.localFlood, err = self.ofSwitch.NewFlood()
	if err != nil {
		return err
	}
	vlan.allFlood, err = self.ofSwitch.NewFlood()
	if err != nil {
		return err
	}

	// Walk all VTEP ports and add vni-vlan mapping for new VNI
	for _, vtepPort := range self.agent.vtepTable {
		// Install a flow entry for  VNI/vlan and point it to macDest table
		portVlanFlow, err := self.vlanTable.NewFlow(ofctrl.FlowMatch{
			Priority:  FLOW_MATCH_PRIORITY,
			InputPort: *vtepPort,
			TunnelId:  uint64(vni),
		})
		if err != nil {
			log.Errorf("Error creating port vlan flow for vlan %d. Err: %v", vlanId, err)
			return err
		}

		// Set vlan id
		portVlanFlow.SetVlan(vlanId)

		// Set the metadata to indicate packet came in from VTEP port
		portVlanFlow.SetMetadata(METADATA_RX_VTEP, METADATA_RX_VTEP)

		// Point to next table
		// Note that we pypass policy lookup on dest host
		portVlanFlow.Next(self.macDestTable)

		// save it in cache
		vlan.vtepVlanFlowDb[*vtepPort] = portVlanFlow
	}

	// Walk all VTEP ports and add it to the allFlood list
	for _, vtepPort := range self.agent.vtepTable {
		output, err := self.ofSwitch.OutputPort(*vtepPort)
		if err != nil {
			return err
		}
		vlan.allFlood.AddTunnelOutput(output, uint64(vni))
	}

	log.Infof("Installing vlan flood entry for vlan: %d", vlanId)

	// Install local mac miss entries in macDestTable
	var metadataLclRx uint64 = 0
	var metadataVtepRx uint64 = METADATA_RX_VTEP
	localMacMiss, err := self.macDestTable.NewFlow(ofctrl.FlowMatch{
		Priority:     FLOW_FLOOD_PRIORITY,
		VlanId:       vlanId,
		Metadata:     &metadataLclRx,
		MetadataMask: &metadataVtepRx,
	})
	if err != nil {
		log.Errorf("Error creating local+remote flood. Err: %v", err)
		return err
	}

	localMacMiss.Next(vlan.allFlood)
	vlan.localMacMiss = localMacMiss

	// Setup Mac miss flow for vtep traffic
	vtepMacMiss, err := self.macDestTable.NewFlow(ofctrl.FlowMatch{
		Priority:     FLOW_FLOOD_PRIORITY,
		VlanId:       vlanId,
		Metadata:     &metadataVtepRx,
		MetadataMask: &metadataVtepRx,
	})
	if err != nil {
		log.Errorf("Error creating local flood. Err: %v", err)
		return err
	}
	vtepMacMiss.Next(vlan.localFlood)
	vlan.vtepMacMiss = vtepMacMiss

	// store it in DB
	self.vlanDb[vlanId] = vlan

	return nil
}

// Remove a vlan
func (self *Vxlan) RemoveVlan(vlanId uint16, vni uint32) error {
	vlan := self.vlanDb[vlanId]
	if vlan == nil {
		log.Fatalf("Could not find the vlan %d", vlanId)
	}

	// Make sure the flood lists are empty
	if vlan.localFlood.NumOutput() != 0 {
		log.Fatalf("VLAN flood list is not empty")
	}

	log.Infof("Deleting vxlan: %d, vlan: %d", vni, vlanId)

	// Uninstall the flood lists
	vlan.allFlood.Delete()
	vlan.localFlood.Delete()

	// Uninstall mac miss Entries
	vlan.localMacMiss.Delete()
	vlan.vtepMacMiss.Delete()

	// uninstall vtep vlan mapping flows
	for _, portVlanFlow := range vlan.vtepVlanFlowDb {
		portVlanFlow.Delete()
	}

	// Remove it from DB
	delete(self.vlanDb, vlanId)

	return nil
}

// AddEndpoint Add an endpoint to the datapath
func (self *Vxlan) AddEndpoint(endpoint *OfnetEndpoint) error {
	log.Infof("Received endpoint: %+v", endpoint)

	// Lookup the VTEP for the endpoint
	vtepPort := self.agent.vtepTable[endpoint.OriginatorIp.String()]
	if vtepPort == nil {
		log.Errorf("Could not find the VTEP for endpoint: %+v", endpoint)

		return errors.New("VTEP not found")
	}

	// map VNI to vlan Id
	vlanId := self.agent.vniVlanMap[endpoint.Vni]
	if vlanId == nil {
		log.Errorf("Endpoint %+v on unknown VNI: %d", endpoint, endpoint.Vni)
		return errors.New("Unknown VNI")
	}

	macAddr, err := net.ParseMAC(endpoint.MacAddrStr)
	if err != nil {
		return err
	}

	// Install the endpoint in OVS
	// Create an output port for the vtep
	outPort, err := self.ofSwitch.OutputPort(*vtepPort)
	if err != nil {
		log.Errorf("Error creating output port %d. Err: %v", *vtepPort, err)
		return err
	}

	// Finally install the mac address
	macFlow, err := self.macDestTable.NewFlow(ofctrl.FlowMatch{
		Priority: FLOW_MATCH_PRIORITY,
		VlanId:   *vlanId,
		MacDa:    &macAddr,
	})
	if err != nil {
		log.Errorf("Error creating mac flow {%+v}. Err: %v", macFlow, err)
		return err
	}

	macFlow.PopVlan()
	macFlow.SetTunnelId(uint64(endpoint.Vni))
	macFlow.Next(outPort)

	// Install dst group entry for the endpoint
	err = self.policyAgent.AddEndpoint(endpoint)
	if err != nil {
		log.Errorf("Error adding endpoint to policy agent{%+v}. Err: %v", endpoint, err)
		return err
	}

	// Save the flow in DB
	self.macFlowDb[endpoint.MacAddrStr] = macFlow
	return nil
}

// RemoveEndpoint removes an endpoint from the datapath
func (self *Vxlan) RemoveEndpoint(endpoint *OfnetEndpoint) error {
	log.Infof("Received DELETE endpoint: %+v", endpoint)

	// find the flow
	macFlow := self.macFlowDb[endpoint.MacAddrStr]
	if macFlow == nil {
		log.Errorf("Could not find the flow for endpoint: %+v", endpoint)
		return errors.New("Mac flow not found")
	}

	// Delete the flow
	err := macFlow.Delete()
	if err != nil {
		log.Errorf("Error deleting mac flow: %+v. Err: %v", macFlow, err)
	}

	// Remove the endpoint from policy tables
	err = self.policyAgent.DelEndpoint(endpoint)
	if err != nil {
		log.Errorf("Error deleting endpoint to policy agent{%+v}. Err: %v", endpoint, err)
		return err
	}

	return nil
}

// initialize Fgraph on the switch
func (self *Vxlan) initFgraph() error {
	sw := self.ofSwitch

	log.Infof("Installing initial flow entries")

	// Create all tables
	self.inputTable = sw.DefaultTable()
	self.vlanTable, _ = sw.NewTable(VLAN_TBL_ID)
	self.macDestTable, _ = sw.NewTable(MAC_DEST_TBL_ID)

	// Init policy tables
	err := self.policyAgent.InitTables(MAC_DEST_TBL_ID)
	if err != nil {
		log.Fatalf("Error installing policy table. Err: %v", err)
		return err
	}

	//Create all drop entries
	// Drop mcast source mac
	bcastMac, _ := net.ParseMAC("01:00:00:00:00:00")
	bcastSrcFlow, _ := self.inputTable.NewFlow(ofctrl.FlowMatch{
		Priority:  FLOW_MATCH_PRIORITY,
		MacSa:     &bcastMac,
		MacSaMask: &bcastMac,
	})
	bcastSrcFlow.Next(sw.DropAction())

	// FIXME: Add additional checks on:
	//  Drop STP packets
	//  Send LLDP packets to controller
	//  Send LACP packets to controller
	//  Drop all other reserved mcast packets in 01-80-C2 range.

	// Send all valid packets to vlan table
	// This is installed at lower priority so that all packets that miss above
	// flows will match entry
	validPktFlow, _ := self.inputTable.NewFlow(ofctrl.FlowMatch{
		Priority: FLOW_MISS_PRIORITY,
	})
	validPktFlow.Next(self.vlanTable)

	// Drop all packets that miss Vlan lookup
	vlanMissFlow, _ := self.vlanTable.NewFlow(ofctrl.FlowMatch{
		Priority: FLOW_MISS_PRIORITY,
	})
	vlanMissFlow.Next(sw.DropAction())

	// Drop all packets that miss mac dest lookup AND vlan flood lookup
	floodMissFlow, _ := self.macDestTable.NewFlow(ofctrl.FlowMatch{
		Priority: FLOW_MISS_PRIORITY,
	})
	floodMissFlow.Next(sw.DropAction())

	// Drop all
	return nil
}
