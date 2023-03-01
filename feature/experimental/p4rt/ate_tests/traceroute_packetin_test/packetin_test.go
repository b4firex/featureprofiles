// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// package to test P4RT with traceroute traffic of IPV4 and IPV6 with TTL/HopLimit as 0&1.
// go test -v . -testbed /root/ondatra/featureprofiles/topologies/atedut_2.testbed -binding /root/ondatra/featureprofiles/topologies/atedut_2.binding -outputs_dir logs

package traceroute_packetin_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/cisco-open/go-p4/p4rt_client"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/openconfig/featureprofiles/feature/experimental/p4rt/internal/p4rtutils"
	"github.com/openconfig/ondatra"
	"github.com/openconfig/ondatra/gnmi"
	p4_v1 "github.com/p4lang/p4runtime/go/p4/v1"
)

type PacketIO interface {
	GetTableEntry(delete bool, IsIpv4 bool) []*p4rtutils.ACLWbbIngressTableEntryInfo
	GetPacketTemplate() *PacketIOPacket
	GetTrafficFlow(ate *ondatra.ATEDevice, isIpv4 bool, TTL uint8, frameSize uint32, frameRate uint64) []*ondatra.Flow
	GetEgressPort() string
	GetIngressPort() string
}

type PacketIOPacket struct {
	TTL            *uint8
	SrcMAC, DstMAC *string
	EthernetType   *uint32
	HopLimit       *uint8
}

type testArgs struct {
	ctx      context.Context
	leader   *p4rt_client.P4RTClient
	follower *p4rt_client.P4RTClient
	dut      *ondatra.DUTDevice
	ate      *ondatra.ATEDevice
	top      *ondatra.ATETopology
	packetIO PacketIO
}

// programmTableEntry programs or deletes p4rt table entry based on delete flag.
func programmTableEntry(client *p4rt_client.P4RTClient, packetIO PacketIO, delete bool, IsIpv4 bool) error {
	err := client.Write(&p4_v1.WriteRequest{
		DeviceId:   deviceID,
		ElectionId: &p4_v1.Uint128{High: uint64(0), Low: electionId},
		Updates: p4rtutils.ACLWbbIngressTableEntryGet(
			packetIO.GetTableEntry(delete, IsIpv4),
		),
		Atomicity: p4_v1.WriteRequest_CONTINUE_ON_ERROR,
	})
	return err
}

// decodePacket decodes L2 header in the packet and returns TTL. packetData[14:0] to remove first 14 bytes of Ethernet header.
func decodePacket4(t *testing.T, packetData []byte) uint8 {
	t.Helper()
	packet := gopacket.NewPacket(packetData[14:], layers.LayerTypeIPv4, gopacket.Default)
	if IPv4 := packet.Layer(layers.LayerTypeIPv4); IPv4 != nil {
		ipv4, _ := IPv4.(*layers.IPv4)
		IPv4 := ipv4.TTL
		return IPv4
	}
	return 7
}

// decodePacket decodes IPV6 L2 header in the packet and returns HopLimit. packetData[14:] to remove first 14 bytes of Ethernet header.
func decodePacket6(t *testing.T, packetData []byte) uint8 {
	t.Helper()
	packet := gopacket.NewPacket(packetData[14:], layers.LayerTypeIPv6, gopacket.Default)
	if IPv6 := packet.Layer(layers.LayerTypeIPv6); IPv6 != nil {
		ipv6, _ := IPv6.(*layers.IPv6)
		IPv6 := ipv6.HopLimit
		return IPv6
	}
	return 7
}

// testTraffic sends traffic flow for duration seconds.
func testTraffic(t *testing.T, ate *ondatra.ATEDevice, flows []*ondatra.Flow, srcEndPoint *ondatra.Interface, duration int) []uint64 {
	t.Helper()
	for _, flow := range flows {
		flow.WithSrcEndpoints(srcEndPoint).WithDstEndpoints(srcEndPoint)
	}
	ate.Traffic().Start(t, flows...)
	time.Sleep(time.Duration(duration) * time.Second)
	ate.Traffic().Stop(t)

	txPackets := []uint64{}
	for _, flow := range flows {
		txPackets = append(txPackets, gnmi.Get(t, ate, gnmi.OC().Flow(flow.Name()).Counters().OutPkts().State()))
	}
	return txPackets
}

// fetchPackets reads p4rt packets sent to p4rt client.
func fetchPackets(ctx context.Context, t *testing.T, client *p4rt_client.P4RTClient, expectNumber int, timeout time.Duration) []*p4rt_client.P4RTPacketInfo {
	t.Helper()
	packets := []*p4rt_client.P4RTPacketInfo{}
	for start := time.Now(); len(packets) < expectNumber && time.Since(start) < timeout; {
		_, packet, err := client.StreamChannelGetPacket(&streamName, 0)
		if err == io.EOF {
			t.Logf("EOF error is seen in PacketIn.")
			break
		} else if err == nil {
			if packet != nil {
				packets = append(packets, packet)
			} else {
				time.Sleep(time.Second)
			}
		} else {
			t.Fatalf("There is error seen when receiving packets. %v, %s", err, err)
			break
		}
	}
	return packets
}

// testPacketIn programs p4rt table entry and sends traffic related to Traceroute,
// then validates packetin message metadata and payload.
func testPacketIn(ctx context.Context, t *testing.T, args *testArgs, IsIpv4 bool) {
	leader := args.leader
	follower := args.follower

	if IsIpv4 {
		// Insert p4rtutils acl entry on the DUT
		if err := programmTableEntry(leader, args.packetIO, false, IsIpv4); err != nil {
			t.Fatalf("There is error when programming entry")
		}
		// Delete p4rtutils acl entry on the device
		defer programmTableEntry(leader, args.packetIO, true, IsIpv4)
	} else {
		// Insert p4rtutils acl entry on the DUT
		if err := programmTableEntry(leader, args.packetIO, false, false); err != nil {
			t.Fatalf("There is error when programming entry")
		}
		// Delete p4rtutils acl entry on the device
		defer programmTableEntry(leader, args.packetIO, true, false)
	}

	// Send Traceroute traffic from ATE
	srcEndPoint := args.top.Interfaces()[atePort1.Name]

	packetInTests := []struct {
		desc       string
		client     *p4rt_client.P4RTClient
		expectPass bool
	}{{
		desc:       "PacketIn to Primary Controller",
		client:     leader,
		expectPass: true,
	}, {
		desc:       "PacketIn to Secondary Controller",
		client:     follower,
		expectPass: false,
	}}

	t.Log("TTL/HopLimit 1")
	txPackets := testTraffic(t, args.ate, args.packetIO.GetTrafficFlow(args.ate, IsIpv4, 1, 300, 2), srcEndPoint, 10)
	for _, test := range packetInTests {
		t.Run(test.desc, func(t *testing.T) {
			// Extract packets from PacketIn message sent to p4rt client
			wantPktCnt := int(txPackets[0])
			packets := fetchPackets(ctx, t, test.client, wantPktCnt, 30*time.Second)

			if !test.expectPass {
				if len(packets) > 0 {
					t.Fatalf("Unexpected packets received.")
					return
				}
				return
			} else {
				if len(packets) != wantPktCnt {
					t.Fatalf("Not all the packets are received, want %v have %v", wantPktCnt, len(packets))
				}
				t.Logf("Start to decode packet and compare with expected packets.")
				wantPacket := args.packetIO.GetPacketTemplate()
				for _, packet := range packets {
					if packet != nil {
						if wantPacket.TTL != nil {
							//TTL/HopLimit comparison for IPV4 & IPV6
							if IsIpv4 {
								captureTTL := decodePacket4(t, packet.Pkt.GetPayload())
								if captureTTL != TTL1 {
									t.Fatalf("Packet in PacketIn message is not matching wanted packet=IPV4 TTL1")
								}

							} else {
								captureHopLimit := decodePacket6(t, packet.Pkt.GetPayload())
								if captureHopLimit != HopLimit1 {
									t.Fatalf("Packet in PacketIn message is not matching wanted packet=IPV6 HopLimit1")
								}
							}
						}

						//Metadata comparision
						if metaData := packet.Pkt.GetMetadata(); metaData != nil {
							if got := metaData[0].GetMetadataId(); got == METADATA_INGRESS_PORT {
								if gotPortID := string(metaData[0].GetValue()); gotPortID != args.packetIO.GetIngressPort() {
									t.Fatalf("Ingress Port Id mismatch: want %s, got %s", args.packetIO.GetIngressPort(), gotPortID)
								}
							} else {
								t.Fatalf("Metadata ingress port mismatch: want %d, got %d", METADATA_INGRESS_PORT, got)
							}

							if got := metaData[1].GetMetadataId(); got == METADATA_EGRESS_PORT {
								if gotPortID := string(metaData[1].GetValue()); gotPortID != args.packetIO.GetEgressPort() {
									t.Fatalf("Egress Port Id mismatch: want %s, got %s", args.packetIO.GetEgressPort(), gotPortID)
								}
							} else {
								t.Fatalf("Metadata egress port mismatch: want %d, got %d", METADATA_EGRESS_PORT, got)
							}
						} else {
							t.Fatalf("Packet missing metadata information.")
						}
					}
				}
			}
		})
	}
}
