package consistence

import (
	"sync"
	"time"

	"github.com/absolute8511/gorpc"
)

type INsqlookupRemoteProxy interface {
	RemoteAddr() string
	Reconnect() error
	Close()
	RequestJoinCatchup(topic string, partition int, nid string) *CoordErr
	RequestJoinTopicISR(topic string, partition int, nid string) *CoordErr
	ReadyForTopicISR(topic string, partition int, nid string, leaderSession *TopicLeaderSession, joinISRSession string) *CoordErr
	RequestLeaveFromISR(topic string, partition int, nid string) *CoordErr
	RequestLeaveFromISRByLeader(topic string, partition int, nid string, leaderSession *TopicLeaderSession) *CoordErr
	RequestNotifyNewTopicInfo(topic string, partition int, nid string)
	RequestCheckTopicConsistence(topic string, partition int)
}

type nsqlookupRemoteProxyCreateFunc func(string, time.Duration) (INsqlookupRemoteProxy, error)

type NsqLookupRpcClient struct {
	sync.Mutex
	remote  string
	timeout time.Duration
	d       *gorpc.Dispatcher
	c       *gorpc.Client
	dc      *gorpc.DispatcherClient
}

func NewNsqLookupRpcClient(addr string, timeout time.Duration) (INsqlookupRemoteProxy, error) {
	c := gorpc.NewTCPClient(addr)
	c.RequestTimeout = timeout
	c.DisableCompression = true
	c.Start()
	d := gorpc.NewDispatcher()
	d.AddService("NsqLookupCoordRpcServer", &NsqLookupCoordRpcServer{})

	return &NsqLookupRpcClient{
		remote:  addr,
		timeout: timeout,
		d:       d,
		c:       c,
		dc:      d.NewServiceClient("NsqLookupCoordRpcServer", c),
	}, nil
}

func (self *NsqLookupRpcClient) Close() {
	self.Lock()
	if self.c != nil {
		self.c.Stop()
		self.c = nil
	}
	self.Unlock()
}

func (self *NsqLookupRpcClient) ShouldRemoved() bool {
	r := false
	self.Lock()
	if self.c != nil {
		r = self.c.ShouldRemoved()
	}
	self.Unlock()
	return r
}

func (self *NsqLookupRpcClient) RemoteAddr() string {
	return self.remote
}

func (self *NsqLookupRpcClient) Reconnect() error {
	self.Lock()
	if self.c != nil {
		self.c.Stop()
	}
	self.c = gorpc.NewTCPClient(self.remote)
	self.c.RequestTimeout = self.timeout
	self.c.DisableCompression = true
	self.c.Start()
	self.dc = self.d.NewServiceClient("NsqLookupCoordRpcServer", self.c)
	self.Unlock()
	return nil
}

func (self *NsqLookupRpcClient) CallFast(method string, arg interface{}) (interface{}, error) {
	reply, err := self.dc.CallTimeout(method, arg, time.Millisecond*10)
	return reply, err
}

func (self *NsqLookupRpcClient) CallWithRetry(method string, arg interface{}) (interface{}, error) {
	retry := 0
	var err error
	var reply interface{}
	for retry < 5 {
		retry++
		reply, err = self.dc.Call(method, arg)
		if err != nil {
			cerr, ok := err.(*gorpc.ClientError)
			if (ok && cerr.Connection) || self.ShouldRemoved() {
				coordLog.Infof("rpc connection closed, error: %v", err)
				connErr := self.Reconnect()
				if connErr != nil {
					return reply, err
				}
			} else {
				return reply, err
			}
		} else {
			if err != nil {
				coordLog.Infof("rpc call %v error: %v", method, err)
			}
			return reply, err
		}
	}
	return reply, err
}

func (self *NsqLookupRpcClient) RequestJoinCatchup(topic string, partition int, nid string) *CoordErr {
	var req RpcReqJoinCatchup
	req.NodeID = nid
	req.TopicName = topic
	req.TopicPartition = partition
	ret, err := self.CallWithRetry("RequestJoinCatchup", &req)
	return convertRpcError(err, ret)
}

func (self *NsqLookupRpcClient) RequestNotifyNewTopicInfo(topic string, partition int, nid string) {
	var req RpcReqNewTopicInfo
	req.NodeID = nid
	req.TopicName = topic
	req.TopicPartition = partition
	self.CallWithRetry("RequestNotifyNewTopicInfo", &req)
}

func (self *NsqLookupRpcClient) RequestJoinTopicISR(topic string, partition int, nid string) *CoordErr {
	var req RpcReqJoinISR
	req.NodeID = nid
	req.TopicName = topic
	req.TopicPartition = partition
	ret, err := self.CallWithRetry("RequestJoinTopicISR", &req)
	return convertRpcError(err, ret)
}

func (self *NsqLookupRpcClient) ReadyForTopicISR(topic string, partition int, nid string, leaderSession *TopicLeaderSession, joinISRSession string) *CoordErr {
	var req RpcReadyForISR
	req.NodeID = nid
	if leaderSession != nil {
		req.LeaderSession = *leaderSession
	}
	req.JoinISRSession = joinISRSession
	req.TopicName = topic
	req.TopicPartition = partition
	ret, err := self.CallWithRetry("ReadyForTopicISR", &req)
	return convertRpcError(err, ret)
}

func (self *NsqLookupRpcClient) RequestLeaveFromISR(topic string, partition int, nid string) *CoordErr {
	var req RpcReqLeaveFromISR
	req.NodeID = nid
	req.TopicName = topic
	req.TopicPartition = partition
	ret, err := self.CallWithRetry("RequestLeaveFromISR", &req)
	return convertRpcError(err, ret)
}

func (self *NsqLookupRpcClient) RequestLeaveFromISRFast(topic string, partition int, nid string) *CoordErr {
	var req RpcReqLeaveFromISR
	req.NodeID = nid
	req.TopicName = topic
	req.TopicPartition = partition
	ret, err := self.CallFast("RequestLeaveFromISR", &req)
	return convertRpcError(err, ret)
}

func (self *NsqLookupRpcClient) RequestLeaveFromISRByLeader(topic string, partition int, nid string, leaderSession *TopicLeaderSession) *CoordErr {
	var req RpcReqLeaveFromISRByLeader
	req.NodeID = nid
	req.TopicName = topic
	req.TopicPartition = partition
	req.LeaderSession = *leaderSession
	ret, err := self.CallWithRetry("RequestLeaveFromISRByLeader", &req)
	return convertRpcError(err, ret)
}

func (self *NsqLookupRpcClient) RequestCheckTopicConsistence(topic string, partition int) {
	var req RpcReqCheckTopic
	req.TopicName = topic
	req.TopicPartition = partition
	self.CallWithRetry("RequestCheckTopicConsistence", &req)
}
