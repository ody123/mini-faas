package core

import (
	"aliyun/serverless/mini-faas/scheduler/model"
	"aliyun/serverless/mini-faas/scheduler/utils/icmap"
	"aliyun/serverless/mini-faas/scheduler/utils/logger"
	"context"
	uuid "github.com/satori/go.uuid"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	nsPb "aliyun/serverless/mini-faas/nodeservice/proto"
	rmPb "aliyun/serverless/mini-faas/resourcemanager/proto"
	cp "aliyun/serverless/mini-faas/scheduler/config"
	pb "aliyun/serverless/mini-faas/scheduler/proto"
	cmap "github.com/orcaman/concurrent-map"
	"github.com/pkg/errors"
)

type RequestStatus struct {
	FunctionName string
	NodeAddress  string
	ContainerId  string
	// ms
	ScheduleAcquireContainerLatency int64
	ScheduleReturnContainerLatency  int64
	FunctionExecutionDuration       int64
	ResponseTime                    int64
	// bytes
	RequireMemory       int64
	MaxMemoryUsage      int64
	ActualRequireMemory int64

	FunctionTimeout int64

	functionStatus *FunctionStatus
	containerInfo  *ContainerInfo
}

type FunctionStatus struct {
	sync.Mutex
	FunctionName         string
	RequireMemory        int64
	ComputeRequireMemory int64
	SendContainerChan    chan *ContainerInfo
	NodeContainerMap     icmap.ConcurrentMap // nodeNo -> ContainerMap
}

type ContainerInfo struct {
	sync.Mutex
	ContainerId         string // container_id
	nodeInfo            *NodeInfo
	AvailableMemInBytes int64
	containerNo         int
	sendTime            int32
	// 用ConcurrentMap读写锁可能会导致删除容器时重复删除
	requests cmap.ConcurrentMap // request_id -> status
}

type LockMap struct {
	sync.Mutex
	Internal icmap.ConcurrentMap
	num      int32
}

type Router struct {
	nodeMap     *LockMap           // no -> NodeInfo instance_id == nodeDesc.ContainerId == nodeId
	functionMap cmap.ConcurrentMap // function_name -> FunctionStatus
	RequestMap  cmap.ConcurrentMap // request_id -> RequestStatus
	rmClient    rmPb.ResourceManagerClient
}

var ReturnContainerChan chan *model.ResponseInfo

func NewRouter(config *cp.Config, rmClient rmPb.ResourceManagerClient) *Router {
	ReturnContainerChan = make(chan *model.ResponseInfo, 1000)
	// 取结构体地址表示实例化
	return &Router{
		nodeMap: &LockMap{
			Internal: icmap.New(),
		},
		functionMap: cmap.New(),
		RequestMap:  cmap.New(),
		rmClient:    rmClient,
	}
}

// 给结构体类型的引用添加方法，相当于添加实例方法，直接给结构体添加方法相当于静态方法
func (r *Router) Start() {
	// Just in case the router has Internal loops.
}

func (r *Router) AcquireContainer(ctx context.Context, req *pb.AcquireContainerRequest) (*pb.AcquireContainerReply, error) {
	// 可用于执行的container
	var res *ContainerInfo

	// 取该函数相关信息
	r.functionMap.SetIfAbsent(req.FunctionName, &FunctionStatus{
		FunctionName:         req.FunctionName,
		RequireMemory:        req.FunctionConfig.MemoryInBytes,
		ComputeRequireMemory: 0,
		SendContainerChan:    make(chan *ContainerInfo, 300),
		NodeContainerMap:     icmap.New(),
	})

	fmObj, _ := r.functionMap.Get(req.FunctionName)
	functionStatus := fmObj.(*FunctionStatus)

	var computeRequireMemory int64

	var err error
	computeRequireMemory = functionStatus.ComputeRequireMemory
	if computeRequireMemory == 0 {
		computeRequireMemory = req.FunctionConfig.MemoryInBytes
		res, err = r.createNewContainer(req, functionStatus, computeRequireMemory)
		if err != nil {
			logger.Warningf("first createNewContainer error: %v", err)
		}
	} else {
		timeout := time.NewTimer(cp.ChannelTimeout)
		select {
		case res = <-functionStatus.SendContainerChan:
			atomic.AddInt32(&(res.sendTime), -1)
			logger.Infof("res id: %s, first wait, use exist container", req.RequestId)
			break
		case <-timeout.C:
			logger.Infof("first wait, timeout")
			break
		}
	}

	if res == nil {
		res = r.getAvailableContainer(functionStatus, computeRequireMemory)
		if res == nil {
			logger.Warningf("getAvailableContainer fail")
			res, err = r.createNewContainer(req, functionStatus, computeRequireMemory)
			if res == nil {
				logger.Warningf("second createNewContainer error: %v", err)
				timeout := time.NewTimer(cp.WaitChannelTimeout)
				now := time.Now().UnixNano()
				select {
				case res = <-functionStatus.SendContainerChan:
					atomic.AddInt32(&(res.sendTime), -1)
					logger.Warningf("second wait latency %d ", (time.Now().UnixNano()-now)/1e6)
					break
				case <-timeout.C:
					break
				}
			}
		}
	}

	if res == nil {
		return nil, err
	}

	res.requests.Set(req.RequestId, 1)
	atomic.AddInt64(&(res.AvailableMemInBytes), -computeRequireMemory)

	requestStatus := &RequestStatus{
		FunctionName:        req.FunctionName,
		NodeAddress:         res.nodeInfo.address,
		ContainerId:         res.ContainerId,
		RequireMemory:       req.FunctionConfig.MemoryInBytes,
		ActualRequireMemory: computeRequireMemory,
		FunctionTimeout:     req.FunctionConfig.TimeoutInMs,

		containerInfo: res,

		functionStatus: functionStatus,
	}

	r.RequestMap.Set(req.RequestId, requestStatus)

	return &pb.AcquireContainerReply{
		NodeId:          res.nodeInfo.nodeID,
		NodeAddress:     res.nodeInfo.address,
		NodeServicePort: res.nodeInfo.port,
		ContainerId:     res.ContainerId,
	}, nil
}

func (r *Router) createNewContainer(req *pb.AcquireContainerRequest, functionStatus *FunctionStatus, actualRequireMemory int64) (*ContainerInfo, error) {
	var res *ContainerInfo
	createContainerErr := errors.Errorf("")
	// 获取一个node，有满足容器内存要求的node直接返回该node，否则申请一个新的node返回
	// 容器大小取多少？
	node, err := r.getNode(req)
	if node == nil {
		createContainerErr = err
	} else {
		atomic.AddInt64(&(node.availableMemInBytes), -req.FunctionConfig.MemoryInBytes)
		node.requests.Set(req.RequestId, 1)

		// 在node上创建运行该函数的容器，并保存容器信息
		ctx, cancel := context.WithTimeout(context.Background(), cp.Timout)
		defer cancel()
		now := time.Now().UnixNano()
		replyC, err := node.CreateContainer(ctx, &nsPb.CreateContainerRequest{
			Name: req.FunctionName + uuid.NewV4().String(),
			FunctionMeta: &nsPb.FunctionMeta{
				FunctionName:  req.FunctionName,
				Handler:       req.FunctionConfig.Handler,
				TimeoutInMs:   req.FunctionConfig.TimeoutInMs,
				MemoryInBytes: req.FunctionConfig.MemoryInBytes,
			},
			RequestId: req.RequestId,
		})
		logger.Infof("%s CreateContainer, Latency: %d", functionStatus.FunctionName, (time.Now().UnixNano()-now)/1e6)
		if replyC == nil {
			// 没有创建成功则删除
			atomic.AddInt64(&(node.availableMemInBytes), req.FunctionConfig.MemoryInBytes)
			node.requests.Remove(req.RequestId)
			createContainerErr = errors.Wrapf(err, "failed to create container on %s", node.address)
		} else {
			functionStatus.NodeContainerMap.SetIfAbsent(node.nodeNo, &LockMap{
				num:      0,
				Internal: icmap.New(),
			})
			nodeContainerMapObj, _ := functionStatus.NodeContainerMap.Get(node.nodeNo)
			nodeContainerMap := nodeContainerMapObj.(*LockMap)
			containerNo := int(atomic.AddInt32(&(nodeContainerMap.num), 1))
			res = &ContainerInfo{
				ContainerId:         replyC.ContainerId,
				nodeInfo:            node,
				containerNo:         containerNo,
				AvailableMemInBytes: req.FunctionConfig.MemoryInBytes - actualRequireMemory,
				requests:            cmap.New(),
			}
			// 新键的容器还没添加进containerMap所以不用锁
			node.containers.Set(res.ContainerId, 1)

			res.requests.Set(req.RequestId, 1)
			nodeContainerMap.Internal.Set(containerNo, res)
			logger.Infof("request id: %s, create container", req.RequestId)
		}
	}
	return res, createContainerErr
}

func (r *Router) getNode(req *pb.AcquireContainerRequest) (*NodeInfo, error) {
	var node *NodeInfo

	node = r.getAvailableNode(req)

	if node != nil {
		logger.Infof("rq id: %s, get exist node: %s", req.RequestId, node.nodeID)
		return node, nil
	} else {

		// 这里是否要加锁？
		// 可能当前读的时候是小于，但是其实有一个节点正在添加中了
		// 达到最大限制直接返回
		if r.nodeMap.Internal.Count() >= cp.MaxNodeNum {
			return nil, errors.Errorf("node maximum limit reached")
		}

		var err error
		node, err = r.reserveNode()

		return node, err
	}
}

func (r *Router) reserveNode() (*NodeInfo, error) {
	// 超时没有请求到节点就取消
	ctxR, cancelR := context.WithTimeout(context.Background(), cp.Timout)
	defer cancelR()
	now := time.Now().UnixNano()
	replyRn, err := r.rmClient.ReserveNode(ctxR, &rmPb.ReserveNodeRequest{})
	latency := (time.Now().UnixNano() - now) / 1e6
	if err != nil {
		logger.Errorf("Failed to reserve node due to %v, Latency: %d", err, latency)
		time.Sleep(100 * time.Millisecond)
		return nil, err
	}
	if replyRn == nil {
		time.Sleep(100 * time.Millisecond)
		return nil, err
	}
	logger.Infof("ReserveNode,NodeAddress: %s, Latency: %d", replyRn.Node.Address, latency)

	nodeDesc := replyRn.Node
	nodeNo := int(atomic.AddInt32(&r.nodeMap.num, 1))
	// 本地ReserveNode 返回的可用memory 比 node.GetStats少了一倍, 比赛环境正常
	node, err := NewNode(nodeDesc.Id, nodeNo, nodeDesc.Address, nodeDesc.NodeServicePort, nodeDesc.MemoryInBytes)
	logger.Infof("ReserveNode memory: %d, nodeNo: %s", nodeDesc.MemoryInBytes, nodeNo)
	if err != nil {
		logger.Errorf("Failed to NewNode %v", err)
		return nil, err
	}

	//用node.GetStats重置可用内存
	//nodeGS, _ := node.GetStats(context.Background(), &nsPb.GetStatsRequest{})
	//node.availableMemInBytes = nodeGS.NodeStats.AvailableMemoryInBytes

	r.nodeMap.Internal.Set(node.nodeNo, node)
	logger.Infof("ReserveNode id: %s", nodeDesc.Id)

	return node, nil
}

// 取满足要求情况下，资源最少的节点，以达到紧密排布, 优先取保留节点
func (r *Router) getAvailableNode(req *pb.AcquireContainerRequest) *NodeInfo {
	var node *NodeInfo
	r.nodeMap.Lock()

	for _, i := range sortedKeys(r.nodeMap.Internal.Keys()) {
		nodeObj, ok := r.nodeMap.Internal.Get(i)
		if ok {
			nowNode := nodeObj.(*NodeInfo)
			availableMemInBytes := nowNode.availableMemInBytes
			if availableMemInBytes > req.FunctionConfig.MemoryInBytes {
				node = nowNode
				break
			}
		}
	}
	r.nodeMap.Unlock()
	return node
}

func (r *Router) getAvailableContainer(functionStatus *FunctionStatus, computeRequireMemory int64) *ContainerInfo {
	for _, i := range sortedKeys(r.nodeMap.Internal.Keys()) {
		containerMapObj, ok := functionStatus.NodeContainerMap.Get(i)
		if ok {
			containerMap := containerMapObj.(*LockMap)
			for _, j := range sortedKeys(containerMap.Internal.Keys()) {
				containerMap.Lock()
				nowContainerObj, ok := containerMap.Internal.Get(j)
				if ok {
					nowContainer := nowContainerObj.(*ContainerInfo)
					if nowContainer.AvailableMemInBytes >= computeRequireMemory {
						containerMap.Unlock()
						return nowContainer
					}
				}
				containerMap.Unlock()
			}
		}
	}
	return nil
}

func (r *Router) ReturnContainer(ctx context.Context, res *model.ResponseInfo) error {
	timeout := time.NewTimer(cp.ChannelTimeout)
	select {
	case ReturnContainerChan <- res:
		break
	case <-timeout.C:
		logger.Warningf("ReturnContainer timeout")
		break
	}
	return nil
}

func sortedKeys(keys []int) []int {
	sort.Ints(keys)
	return keys
}
