package tv

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	serviceType = "_hongguotv._tcp.local."
	mdnsAddress = "224.0.0.251:5353"
	protocolV1 = 1
)

type Device struct {
	DeviceID string `json:"device_id"`
	Name string `json:"name"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Model string `json:"model,omitempty"`
	Code string `json:"code,omitempty"`
	ServiceName string `json:"service_name"`
	Host string `json:"host"`
	Port int `json:"port"`
}

type HandoffRequest struct {
	DeviceID string `json:"device_id"`
	SeriesID string `json:"series_id"`
	Episode int `json:"episode"`
	PositionMS int64 `json:"position_ms"`
}

type HandoffResult struct {
	Accepted bool `json:"accepted"`
	Message string `json:"message"`
	DeviceName string `json:"device_name"`
}

type Service struct {
	mu sync.Mutex
	devices map[string]Device
	seenAt map[string]time.Time
}

func New() *Service {
	return &Service{devices:make(map[string]Device),seenAt:make(map[string]time.Time)}
}

type srvRecord struct { host string; port int }
type discoveryState struct {
	instances map[string]bool
	srvs map[string]srvRecord
	txt map[string]map[string]string
	addresses map[string]string
}

func newDiscoveryState() *discoveryState {
	return &discoveryState{
		instances:make(map[string]bool),
		srvs:make(map[string]srvRecord),
		txt:make(map[string]map[string]string),
		addresses:make(map[string]string),
	}
}

func (s *Service) Discover(ctx context.Context, duration time.Duration) ([]Device,error) {
	if duration<=0 || duration>5*time.Second {duration=2500*time.Millisecond}
	ctx,cancel:=context.WithTimeout(ctx,duration)
	defer cancel()
	interfaces,err:=net.Interfaces()
	if err!=nil {return nil,err}
	group:=&net.UDPAddr{IP:net.ParseIP("224.0.0.251"),Port:5353}
	connections:=make([]*net.UDPConn,0)
	for _,iface:=range interfaces {
		if iface.Flags&net.FlagUp==0 || iface.Flags&net.FlagMulticast==0 || iface.Flags&net.FlagLoopback!=0 {continue}
		hasIPv4:=false
		addrs,_:=iface.Addrs()
		for _,addr:=range addrs {
			if ipnet,ok:=addr.(*net.IPNet);ok && ipnet.IP.To4()!=nil && !ipnet.IP.IsLoopback() {hasIPv4=true;break}
		}
		if !hasIPv4 {continue}
		conn,listenErr:=net.ListenMulticastUDP("udp4",&iface,group)
		if listenErr==nil {
			_ = conn.SetReadBuffer(64<<10)
			connections=append(connections,conn)
		}
	}
	if len(connections)==0 {return nil,fmt.Errorf("找不到可用于 mDNS 的局域网网卡")}

	defer func(){for _,conn:=range connections{_ = conn.Close()}}()
	packets:=make(chan []byte,64)
	for _,conn:=range connections {
		go func(c *net.UDPConn) {
			buffer:=make([]byte,9000)
			for {
				_ = c.SetReadDeadline(time.Now().Add(300*time.Millisecond))
				n,_,readErr:=c.ReadFromUDP(buffer)
				if readErr!=nil {
					if ne,ok:=readErr.(net.Error);ok && ne.Timeout() {
						select {case <-ctx.Done():return;default:continue}
					}
					return
				}
				packet:=append([]byte(nil),buffer[:n]...)
				select {case packets<-packet:case <-ctx.Done():return}
			}
		}(conn)
	}

	query:=buildPTRQuery(serviceType)
	ticker:=time.NewTicker(650*time.Millisecond)
	defer ticker.Stop()
	timer:=time.NewTimer(duration)
	defer timer.Stop()
	state:=newDiscoveryState()
	send:=func(){
		for _,conn:=range connections {_,_=conn.WriteToUDP(query,group)}
	}
	send()
loop:
	for {
		select {
		case packet:=<-packets:
			_ = parsePacket(packet,state)
		case <-ticker.C:
			send()
		case <-timer.C:
			break loop
		case <-ctx.Done():
			return nil,ctx.Err()
		}
	}
	devices:=state.devices()
	s.mu.Lock()
	now:=time.Now()
	for id,seen:=range s.seenAt {if now.Sub(seen)>2*time.Minute {delete(s.seenAt,id);delete(s.devices,id)}}
	for _,device:=range devices {s.devices[device.DeviceID]=device;s.seenAt[device.DeviceID]=now}
	s.mu.Unlock()
	return devices,nil
}

func (state *discoveryState) devices() []Device {
	out:=make([]Device,0,len(state.instances))
	for instance:=range state.instances {
		srv,ok:=state.srvs[instance]
		if !ok || srv.port<1 || srv.port>65535 {continue}
		attrs:=state.txt[instance]
		if attrs["protocol"]!=fmt.Sprint(protocolV1) {continue}
		deviceID:=strings.TrimSpace(attrs["deviceid"])
		if deviceID=="" {continue}
		host:=state.addresses[srv.host]
		if host=="" {continue}
		name:=strings.TrimSpace(attrs["label"])
		if name=="" {name=strings.TrimSuffix(strings.Split(instance,".")[0],".")}
		out=append(out,Device{
			DeviceID:deviceID,Name:name,Manufacturer:attrs["manufacturer"],Model:attrs["model"],
			Code:attrs["code"],ServiceName:instance,Host:host,Port:srv.port,
		})
	}
	sort.Slice(out,func(i,j int)bool{return strings.ToLower(out[i].Name)<strings.ToLower(out[j].Name)})
	return out
}

func (s *Service) Handoff(ctx context.Context,request HandoffRequest)(HandoffResult,error) {
	if request.DeviceID=="" || len(request.SeriesID)==0 || len(request.SeriesID)>256 ||
		request.Episode<1 || request.Episode>100000 || request.PositionMS<0 || request.PositionMS>86400000 {
		return HandoffResult{},fmt.Errorf("投屏请求参数无效")
	}
	s.mu.Lock()
	device,ok:=s.devices[request.DeviceID]
	seen:=s.seenAt[request.DeviceID]
	s.mu.Unlock()
	if !ok || time.Since(seen)>2*time.Minute {return HandoffResult{},fmt.Errorf("电视设备列表已过期，请重新搜索")}
	ip,err:=netip.ParseAddr(device.Host)
	if err!=nil || !(ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		return HandoffResult{},fmt.Errorf("电视地址不是局域网地址")
	}
	random:=make([]byte,16)
	if _,err:=rand.Read(random);err!=nil {return HandoffResult{},err}
	transferID:=hex.EncodeToString(random)
	payload:=map[string]any{
		"protocol":protocolV1,"type":"play","transferId":transferID,"targetDeviceId":device.DeviceID,
		"seriesId":request.SeriesID,"episode":request.Episode,"positionMs":request.PositionMS,
	}
	body,err:=json.Marshal(payload)
	if err!=nil {return HandoffResult{},err}
	body=append(body,10)
	dialer:=net.Dialer{Timeout:4*time.Second}
	conn,err:=dialer.DialContext(ctx,"tcp",net.JoinHostPort(device.Host,fmt.Sprint(device.Port)))
	if err!=nil {return HandoffResult{},fmt.Errorf("连接电视失败：%w",err)}
	defer conn.Close()
	deadline:=time.Now().Add(40*time.Second)
	if ctxDeadline,ok:=ctx.Deadline();ok && ctxDeadline.Before(deadline) {deadline=ctxDeadline}
	_ = conn.SetDeadline(deadline)
	for len(body)>0 {
		n,writeErr:=conn.Write(body)
		if n>0 {body=body[n:]}
		if writeErr!=nil {return HandoffResult{},fmt.Errorf("发送投屏请求失败：%w",writeErr)}
		if n==0 {return HandoffResult{},io.ErrShortWrite}
	}
	stopCancel:=context.AfterFunc(ctx,func(){_ = conn.Close()})
	defer stopCancel()
	reader:=bufio.NewReaderSize(io.LimitReader(conn,8193),8192)
	line,err:=reader.ReadString(10)
	if err!=nil {return HandoffResult{},fmt.Errorf("电视没有返回投屏结果：%w",err)}
	if len(line)>8192 {return HandoffResult{},fmt.Errorf("电视返回的数据超出限制")}
	var reply struct {
		Protocol int `json:"protocol"`
		TransferID string `json:"transferId"`
		Accepted bool `json:"accepted"`
		Message string `json:"message"`
		DeviceID string `json:"deviceId"`
	}
	if err:=json.Unmarshal([]byte(line),&reply);err!=nil {return HandoffResult{},fmt.Errorf("电视返回的数据格式无效")}
	if reply.Protocol!=protocolV1 || reply.TransferID!=transferID || (reply.DeviceID!="" && reply.DeviceID!=device.DeviceID) {
		return HandoffResult{},fmt.Errorf("电视回执校验失败，播放结果未知")
	}
	message:=strings.TrimSpace(reply.Message)
	if message=="" {if reply.Accepted {message="电视已确认开始播放"} else {message="电视暂时无法播放"}}
	return HandoffResult{Accepted:reply.Accepted,Message:message,DeviceName:device.Name},nil
}

func buildPTRQuery(name string) []byte {
	packet:=make([]byte,12)
	packet[4]=0
	packet[5]=1
	packet=appendDNSName(packet,name)
	packet=append(packet,0,12,0,1)
	return packet
}

func appendDNSName(dst []byte,name string) []byte {
	name=strings.TrimSuffix(name,".")
	for _,label:=range strings.Split(name,".") {
		if len(label)==0 {continue}
		dst=append(dst,byte(len(label)))
		dst=append(dst,[]byte(label)...)
	}
	return append(dst,0)
}

func readDNSName(packet []byte,offset int)(string,int,error) {
	if offset<0 || offset>=len(packet) {return "",offset,fmt.Errorf("DNS name offset out of bounds")}
	labels:=make([]string,0,6)
	position:=offset
	next:=-1
	jumps:=0
	for {
		if position>=len(packet) {return "",offset,fmt.Errorf("truncated DNS name")}
		length:=int(packet[position])
		if length&0xc0==0xc0 {
			if position+1>=len(packet) {return "",offset,fmt.Errorf("truncated DNS pointer")}
			pointer:=((length&0x3f)<<8)|int(packet[position+1])
			if pointer>=len(packet) || pointer==position {return "",offset,fmt.Errorf("invalid DNS pointer")}
			if next<0 {next=position+2}
			position=pointer
			jumps++
			if jumps>20 {return "",offset,fmt.Errorf("too many DNS compression pointers")}
			continue
		}
		position++
		if length==0 {
			if next<0 {next=position}
			break
		}
		if length&0xc0!=0 || length>63 || position+length>len(packet) {return "",offset,fmt.Errorf("invalid DNS label")}
		labels=append(labels,string(packet[position:position+length]))
		position+=length
	}
	return strings.ToLower(strings.Join(labels,".")+"."),next,nil
}

func parsePacket(packet []byte,state *discoveryState) error {
	if len(packet)<12 {return fmt.Errorf("short DNS packet")}
	questions:=int(packet[4])<<8|int(packet[5])
	answers:=int(packet[6])<<8|int(packet[7])
	authorities:=int(packet[8])<<8|int(packet[9])
	additionals:=int(packet[10])<<8|int(packet[11])
	records:=answers+authorities+additionals
	offset:=12
	for i:=0;i<questions;i++ {
		_,next,err:=readDNSName(packet,offset);if err!=nil{return err}
		offset=next+4
		if offset>len(packet){return fmt.Errorf("truncated DNS question")}
	}
	for i:=0;i<records;i++ {
		name,next,err:=readDNSName(packet,offset);if err!=nil{return err}
		offset=next
		if offset+10>len(packet){return fmt.Errorf("truncated DNS resource record")}
		typ:=int(packet[offset])<<8|int(packet[offset+1])
		length:=int(packet[offset+8])<<8|int(packet[offset+9])
		offset+=10
		if length<0 || offset+length>len(packet){return fmt.Errorf("truncated DNS RDATA")}
		dataStart:=offset
		switch typ {
		case 12: // PTR
			target,_,err:=readDNSName(packet,dataStart);if err==nil && name==serviceType {state.instances[target]=true}
		case 33: // SRV
			if length>=7 {
				port:=int(packet[dataStart+4])<<8|int(packet[dataStart+5])
				target,_,nameErr:=readDNSName(packet,dataStart+6)
				if nameErr==nil {state.srvs[name]=srvRecord{host:target,port:port}}
			}
		case 16: // TXT
			attrs:=make(map[string]string)
			pos:=dataStart
			for pos<offset+length {
				n:=int(packet[pos]);pos++
				if pos+n>offset+length {break}
				part:=string(packet[pos:pos+n]);pos+=n
				key,value,ok:=strings.Cut(part,"=")
				if ok {attrs[strings.ToLower(key)]=value}
			}
			state.txt[name]=attrs
		case 1: // A
			if length==4 {state.addresses[name]=netip.AddrFrom4([4]byte{packet[dataStart],packet[dataStart+1],packet[dataStart+2],packet[dataStart+3]}).String()}
		}
		offset+=length
	}
	return nil
}
