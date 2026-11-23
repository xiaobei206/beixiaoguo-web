package tv

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func dnsName(name string) []byte {return appendDNSName(nil,name)}
func rr(name string,typ uint16,data []byte) []byte {
	out:=dnsName(name)
	var head [10]byte
	binary.BigEndian.PutUint16(head[0:2],typ)
	binary.BigEndian.PutUint16(head[2:4],1)
	binary.BigEndian.PutUint32(head[4:8],120)
	binary.BigEndian.PutUint16(head[8:10],uint16(len(data)))
	return append(append(out,head[:]...),data...)
}

func TestParseDNSDiscoveryRecords(t *testing.T) {
	service:=serviceType
	instance:="hongguoTV-1234._hongguotv._tcp.local."
	host:="tv.local."
	ptr:=dnsName(instance)
	srv:=make([]byte,6)
	binary.BigEndian.PutUint16(srv[4:6],43210)
	srv=append(srv,dnsName(host)...)
	txt:=[]byte{}
	for _,v:=range []string{"deviceid=tv-123","label=客厅电视","manufacturer=Test","model=TV","code=1234","protocol=1"} {
		txt=append(txt,byte(len(v)))
		txt=append(txt,v...)
	}
	a:=[]byte{192,168,1,88}
	packet:=make([]byte,12)
	binary.BigEndian.PutUint16(packet[6:8],4)
	packet=append(packet,rr(service,12,ptr)...)
	packet=append(packet,rr(instance,33,srv)...)
	packet=append(packet,rr(instance,16,txt)...)
	packet=append(packet,rr(host,1,a)...)
	state:=newDiscoveryState()
	if err:=parsePacket(packet,state);err!=nil {t.Fatal(err)}
	devices:=state.devices()
	if len(devices)!=1 {t.Fatalf("found %d devices",len(devices))}
	device:=devices[0]
	if device.DeviceID!="tv-123" || device.Host!="192.168.1.88" || device.Port!=43210 || device.Name!="客厅电视" {
		t.Fatalf("unexpected device: %+v",device)
	}
}

func TestBuildPTRQuery(t *testing.T) {
	packet:=buildPTRQuery(serviceType)
	if len(packet)<12 || packet[5]!=1 || packet[len(packet)-4]!=0 || packet[len(packet)-3]!=12 {
		t.Fatalf("invalid query packet: %v",packet)
	}
}

func TestHandoffRequiresFreshDiscoveredPrivateTV(t *testing.T) {
	s:=New()
	s.devices["bad"]=Device{DeviceID:"bad",Host:"8.8.8.8",Port:1234}
	s.seenAt["bad"]=time.Now()
	_,err:=s.Handoff(context.Background(),HandoffRequest{DeviceID:"bad",SeriesID:"series",Episode:1})
	if err==nil {t.Fatal("expected public IP to be rejected")}
}

func TestLocalHostIPParsing(t *testing.T) {
	ip:=net.ParseIP("192.168.1.2")
	if ip==nil || !ip.IsPrivate() {t.Fatal("private IP test failed")}
}

func TestMulticastInterfaceSelectionRequiresIPv4(t *testing.T) {
	iface:=net.Interface{Name:"without-ipv4"}
	conn,err:=net.ListenUDP("udp4",&net.UDPAddr{IP:net.IPv4(127,0,0,1)})
	if err!=nil {t.Skipf("cannot open loopback UDP socket: %v",err)}
	defer conn.Close()
	if err:=setMulticastInterface(conn,iface);err==nil {
		t.Fatal("expected interface without IPv4 address to be rejected")
	}
}
