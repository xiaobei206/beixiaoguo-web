package tv

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"io"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"syscall"
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
	logger *log.Logger
}

func New(loggers ...*log.Logger) *Service {
	logger:=log.Default()
	if len(loggers)>0 && loggers[0]!=nil {logger=loggers[0]}
	return &Service{devices:make(map[string]Device),seenAt:make(map[string]time.Time),logger:logger}
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
	if duration<=0 || duration>5*time.Second {duration=3200*time.Millisecond}
	interfaces,err:=net.Interfaces()
	if err!=nil {return nil,fmt.Errorf("读取网络接口失败：%w",err)}
	group:=&net.UDPAddr{IP:net.ParseIP("224.0.0.251"),Port:5353}
	type multicastConn struct { name string; conn *net.UDPConn }
	connections:=make([]multicastConn,0)
	var setupFailures []string
	for _,iface:=range interfaces {
		if iface.Flags&net.FlagUp==0 || iface.Flags&net.FlagMulticast==0 || iface.Flags&net.FlagLoopback!=0 {continue}
		hasIPv4:=false
		addrs,addrErr:=iface.Addrs()
		if addrErr!=nil {
			setupFailures=append(setupFailures,iface.Name+": "+addrErr.Error())
			continue
		}
		for _,address:=range addrs {
			switch network:=address.(type) {
			case *net.IPNet:
				if ip:=network.IP.To4();ip!=nil && !ip.IsLoopback() {hasIPv4=true}
			case *net.IPAddr:
				if ip:=network.IP.To4();ip!=nil && !ip.IsLoopback() {hasIPv4=true}
			}
			if hasIPv4 {break}
		}
		if !hasIPv4 {continue}
		conn,listenErr:=net.ListenMulticastUDP("udp4",&iface,group)
		if listenErr!=nil {
			setupFailures=append(setupFailures,iface.Name+": listen: "+listenErr.Error())
			continue
		}
		if interfaceErr:=setMulticastInterface(conn,iface);interfaceErr!=nil {
			_ = conn.Close()
			setupFailures=append(setupFailures,iface.Name+": multicast interface: "+interfaceErr.Error())
			continue
		}
		_ = conn.SetReadBuffer(128<<10)
		_ = conn.SetWriteBuffer(32<<10)
		connections=append(connections,multicastConn{name:iface.Name,conn:conn})
	}
	if len(connections)==0 {
		return nil,fmt.Errorf("N1 没有可用的 IPv4 mDNS 组播网卡（%s）",strings.Join(setupFailures,"; "))
	}
	defer func(){for _,entry:=range connections{_ = entry.conn.Close()}}()

	// Use a plain child context and a separate timer. A context deadline and a
	// timer with the same duration race in select and used to turn normal scan
	// completion into a spurious deadline error.
	scanCtx,cancel:=context.WithCancel(ctx)
	defer cancel()
	packets:=make(chan []byte,128)
	for _,entry:=range connections {
		go func(c *net.UDPConn) {
			buffer:=make([]byte,9000)
			for {
				_ = c.SetReadDeadline(time.Now().Add(300*time.Millisecond))
				n,_,readErr:=c.ReadFromUDP(buffer)
				if readErr!=nil {
					if ne,ok:=readErr.(net.Error);ok && ne.Timeout() {
						select {case <-scanCtx.Done():return;default:continue}
					}
					return
				}
				packet:=append([]byte(nil),buffer[:n]...)
				select {case packets<-packet:case <-scanCtx.Done():return}
			}
		}(entry.conn)
	}

	type dnsQuery struct { name string; qtype uint16 }
	state:=newDiscoveryState()
	queryAttempts:=0
	sendQueries:=func() error {
		queries:=[]dnsQuery{{name:serviceType,qtype:12}}
		seen:=map[string]bool{serviceType+"/12":true}
		addQuery:=func(name string,qtype uint16) {
			key:=name+"/"+fmt.Sprint(qtype)
			if name!="" && !seen[key] {seen[key]=true;queries=append(queries,dnsQuery{name:name,qtype:qtype})}
		}
		instances:=make([]string,0,len(state.instances))
		for instance:=range state.instances {instances=append(instances,instance)}
		sort.Strings(instances)
		for _,instance:=range instances {
			// A PTR answer is not guaranteed to carry SRV/TXT/A in the same
			// packet. Query the service instance explicitly, as NsdManager does.
			addQuery(instance,33) // SRV
			addQuery(instance,16) // TXT
		}
		hosts:=make([]string,0,len(state.srvs))
		hostSeen:=make(map[string]bool)
		for _,srv:=range state.srvs {
			if srv.host!="" && !hostSeen[srv.host] {hostSeen[srv.host]=true;hosts=append(hosts,srv.host)}
		}
		sort.Strings(hosts)
		for _,host:=range hosts {
			addQuery(host,1)  // A
			addQuery(host,28) // AAAA
		}
		sent:=0
		var firstErr error
		for _,entry:=range connections {
			for _,query:=range queries {
				packet:=buildDNSQuery(query.name,query.qtype)
				if _,writeErr:=entry.conn.WriteToUDP(packet,group);writeErr!=nil {
					if firstErr==nil {firstErr=writeErr}
					s.logger.Printf("mDNS query failed interface=%s name=%q type=%d error=%v",entry.name,query.name,query.qtype,writeErr)
					continue
				}
				sent++
				queryAttempts++
			}
		}
		if sent==0 {
			if firstErr!=nil {return fmt.Errorf("发送 mDNS 组播查询失败：%w",firstErr)}
			return fmt.Errorf("没有向任何局域网网卡发送 mDNS 查询")
		}
		return nil
	}
	if err:=sendQueries();err!=nil {return nil,err}

	ticker:=time.NewTicker(650*time.Millisecond)
	defer ticker.Stop()
	timer:=time.NewTimer(duration)
	defer timer.Stop()
	received:=0
	parseErrors:=0
	lastParseError:=""
loop:
	for {
		select {
		case packet:=<-packets:
			received++
			if parseErr:=parsePacket(packet,state);parseErr!=nil {
				parseErrors++
				lastParseError=parseErr.Error()
			}
			// Resolve newly learned PTR/SRV records immediately rather than
			// assuming a single multicast response contains every RR.
			if err:=sendQueries();err!=nil {
				s.logger.Printf("mDNS follow-up query error: %v",err)
			}
		case <-ticker.C:
			if err:=sendQueries();err!=nil {
				s.logger.Printf("mDNS retry query error: %v",err)
			}
		case <-timer.C:
			break loop
		case <-scanCtx.Done():
			return nil,scanCtx.Err()
		}
	}

	devices:=state.devices()
	s.mu.Lock()
	now:=time.Now()
	for id,seen:=range s.seenAt {
		if now.Sub(seen)>2*time.Minute {delete(s.seenAt,id);delete(s.devices,id)}
	}
	for _,device:=range devices {
		s.devices[device.DeviceID]=device
		s.seenAt[device.DeviceID]=now
	}
	s.mu.Unlock()
	s.logger.Printf("mDNS discovery finished interfaces=%d queries=%d packets=%d parse_errors=%d instances=%d srv=%d txt=%d addresses=%d devices=%d last_parse_error=%q",
		len(connections),queryAttempts,received,parseErrors,len(state.instances),len(state.srvs),len(state.txt),len(state.addresses),len(devices),lastParseError)
	return devices,nil
}

func setMulticastInterface(conn *net.UDPConn,iface net.Interface) error {
	var ipv4 net.IP
	addresses,err:=iface.Addrs()
	if err!=nil {return err}
	for _,address:=range addresses {
		if network,ok:=address.(*net.IPNet);ok {
			if ip:=network.IP.To4();ip!=nil && !ip.IsLoopback() {
				ipv4=ip
				break
			}
		}
	}
	if ipv4==nil {return fmt.Errorf("interface %s has no usable IPv4 address",iface.Name)}
	raw,err:=conn.SyscallConn()
	if err!=nil {return err}
	var socketErr error
	if err:=raw.Control(func(fd uintptr) {
		var value [4]byte
		copy(value[:],ipv4)
		socketErr=syscall.SetsockoptInet4Addr(int(fd),syscall.IPPROTO_IP,syscall.IP_MULTICAST_IF,value)
	});err!=nil {return err}
	return socketErr
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

func buildPTRQuery(name string) []byte { return buildDNSQuery(name,12) }

func buildDNSQuery(name string,qtype uint16) []byte {
	packet:=make([]byte,12)
	packet[5]=1 // one question
	packet=appendDNSName(packet,name)
	packet=append(packet,byte(qtype>>8),byte(qtype),0,1) // QTYPE, QCLASS=IN
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
		case 28: // AAAA; keep an IPv4 answer if one was already received.
			if length==16 && state.addresses[name]=="" {
				var address [16]byte
				copy(address[:],packet[dataStart:offset])
				state.addresses[name]=netip.AddrFrom16(address).String()
			}
		}
		offset+=length
	}
	return nil
}
