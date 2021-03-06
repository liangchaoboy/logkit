package sender

import (
	"bytes"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/qiniu/log"
	"github.com/qiniu/logkit/conf"
	"github.com/qiniu/logkit/queue"
	"github.com/qiniu/logkit/utils"
	"github.com/qiniu/pandora-go-sdk/base/reqerr"
)

const (
	mb                = 1024 * 1024 // 1MB
	defaultWriteLimit = 10          // 默认写速限制为10MB
	maxBytesPerFile   = 100 * mb
	qNameSuffix       = "_local_save"
	memoryChanSuffix  = "_memory"
	defaultMaxProcs   = 1 // 默认没有并发
)

// 可选参数 fault_tolerant 为true的话，以下必填
const (
	KeyFtSyncEvery         = "ft_sync_every"    // 该参数设置多少次写入会同步一次offset log
	KeyFtSaveLogPath       = "ft_save_log_path" // disk queue 数据日志路径
	KeyFtWriteLimit        = "ft_write_limit"   // 写入速度限制，单位MB
	KeyFtStrategy          = "ft_strategy"      // ft 的策略
	KeyFtProcs             = "ft_procs"         // ft并发数，当always_save 策略时启用
	KeyFtMemoryChannel     = "ft_memory_channel"
	KeyFtMemoryChannelSize = "ft_memory_channel_size"
)

// ft 策略
const (
	// KeyFtStrategyBackupOnly 只在失败的时候进行容错
	KeyFtStrategyBackupOnly = "backup_only"
	// KeyFtStrategyAlwaysSave 所有数据都进行容错
	KeyFtStrategyAlwaysSave = "always_save"
)

// FtSender fault tolerance sender wrapper
type FtSender struct {
	stopped     int32
	exitChan    chan struct{}
	innerSender Sender
	logQueue    queue.BackendQueue
	backupQueue queue.BackendQueue
	writeLimit  int  // 写入速度限制，单位MB
	backupOnly  bool // 是否只使用backup queue
	procs       int  //发送并发数
	se          *utils.StatsError
	runnerName  string
	opt         *FtOption
}

type FtOption struct {
	saveLogPath       string
	syncEvery         int64
	writeLimit        int
	backupOnly        bool
	procs             int
	memoryChannel     bool
	memoryChannelSize int
}

type datasContext struct {
	Datas []Data `json:"datas"`
}

// NewFtSender Fault tolerant sender constructor
func NewFtSender(sender Sender, conf conf.MapConf) (*FtSender, error) {
	memoryChannel, _ := conf.GetBoolOr(KeyFtMemoryChannel, false)
	memoryChannelSize, _ := conf.GetIntOr(KeyFtMemoryChannelSize, 100)

	logpath, err := conf.GetString(KeyFtSaveLogPath)
	if !memoryChannel && err != nil {
		return nil, err
	}
	syncEvery, _ := conf.GetIntOr(KeyFtSyncEvery, DefaultFtSyncEvery)
	writeLimit, _ := conf.GetIntOr(KeyFtWriteLimit, defaultWriteLimit)
	strategy, _ := conf.GetStringOr(KeyFtStrategy, KeyFtStrategyAlwaysSave)
	procs, _ := conf.GetIntOr(KeyFtProcs, defaultMaxProcs)
	runnerName, _ := conf.GetStringOr(KeyRunnerName, UnderfinedRunnerName)

	opt := &FtOption{
		saveLogPath:       logpath,
		syncEvery:         int64(syncEvery),
		writeLimit:        writeLimit,
		backupOnly:        strategy == KeyFtStrategyBackupOnly,
		procs:             procs,
		memoryChannel:     memoryChannel,
		memoryChannelSize: memoryChannelSize,
	}

	return newFtSender(sender, runnerName, opt)
}

func newFtSender(innerSender Sender, runnerName string, opt *FtOption) (*FtSender, error) {
	var lq, bq queue.BackendQueue
	if !opt.memoryChannel {
		err := utils.CreateDirIfNotExist(opt.saveLogPath)
		if err != nil {
			return nil, err
		}

		lq = queue.NewDiskQueue("stream"+qNameSuffix, opt.saveLogPath, maxBytesPerFile, 0, maxBytesPerFile, opt.syncEvery, opt.syncEvery, time.Second*2, opt.writeLimit*mb)
		bq = queue.NewDiskQueue("backup"+qNameSuffix, opt.saveLogPath, maxBytesPerFile, 0, maxBytesPerFile, opt.syncEvery, opt.syncEvery, time.Second*2, opt.writeLimit*mb)
	} else {
		lq = queue.NewMemoryQueue("steam"+memoryChanSuffix, opt.memoryChannelSize)
		bq = queue.NewMemoryQueue("backup"+memoryChanSuffix, opt.memoryChannelSize)
	}
	ftSender := FtSender{
		exitChan:    make(chan struct{}),
		innerSender: innerSender,
		logQueue:    lq,
		backupQueue: bq,
		writeLimit:  opt.writeLimit,
		backupOnly:  opt.backupOnly,
		procs:       opt.procs,
		se:          &utils.StatsError{Ft: true},
		runnerName:  runnerName,
	}
	go ftSender.asyncSendLogFromDiskQueue()
	return &ftSender, nil
}

func (ft *FtSender) Name() string {
	return ft.innerSender.Name() + "(ft)"
}

func (ft *FtSender) Send(datas []Data) error {
	if ft.backupOnly {
		// 尝试直接发送数据，当数据失败的时候会加入到本地重试队列。外部不需要重试
		backDataContext, err := ft.trySendDatas(datas, 1)
		if err != nil {
			log.Warnf("Runner[%v] Sender[%v] try Send Datas err: %v", ft.runnerName, ft.innerSender.Name(), err)
			ft.se.AddErrors()
		} else {
			ft.se.AddSuccess()
		}
		// 容错队列会保证重试，此处不向外部暴露发送错误信息
		ft.se.ErrorDetail = nil
		ft.se.Ftlag = ft.backupQueue.Depth()
		if backDataContext != nil {
			var nowDatas []Data
			for _, v := range backDataContext {
				nowDatas = append(nowDatas, v.Datas...)
			}
			if nowDatas != nil {
				ft.se.ErrorDetail = reqerr.NewSendError("save data to backend queue error", ConvertDatasBack(nowDatas), reqerr.TypeDefault)
			}
		}
	} else {
		err := ft.saveToFile(datas)
		if err != nil {
			ft.se.ErrorDetail = err
		} else {
			ft.se.ErrorDetail = nil
		}
		ft.se.Ftlag = ft.backupQueue.Depth() + ft.logQueue.Depth()
	}
	return ft.se
}

func (ft *FtSender) Close() error {
	atomic.AddInt32(&ft.stopped, 1)
	log.Warnf("Runner[%v] wait for Sender[%v] to completely exit", ft.runnerName, ft.Name())
	// 等待错误恢复流程退出
	<-ft.exitChan
	// 等待正常发送流程退出
	for i := 0; i < ft.procs; i++ {
		<-ft.exitChan
	}

	log.Warnf("Runner[%v] Sender[%v] has been completely exited", ft.runnerName, ft.Name())

	// persist queue's meta data
	ft.logQueue.Close()
	ft.backupQueue.Close()

	return ft.innerSender.Close()
}

// marshalData 将数据序列化
func (ft *FtSender) marshalData(datas []Data) (bs []byte, err error) {
	ctx := new(datasContext)
	ctx.Datas = datas
	bs, err = json.Marshal(ctx)
	if err != nil {
		err = reqerr.NewSendError("Cannot marshal data :"+err.Error(), ConvertDatasBack(datas), reqerr.TypeDefault)
		return
	}
	return
}

// unmarshalData 如何将数据从磁盘中反序列化出来
func (ft *FtSender) unmarshalData(dat []byte) (datas []Data, err error) {
	ctx := new(datasContext)
	d := json.NewDecoder(bytes.NewReader(dat))
	d.UseNumber()
	err = d.Decode(&ctx)
	if err != nil {
		return
	}
	datas = ctx.Datas
	return
}

func (ft *FtSender) saveToFile(datas []Data) error {
	bs, err := ft.marshalData(datas)
	if err != nil {
		return err
	}
	err = ft.logQueue.Put(bs)
	if err != nil {
		return reqerr.NewSendError(ft.innerSender.Name()+" Cannot put data into backendQueue: "+err.Error(), ConvertDatasBack(datas), reqerr.TypeDefault)
	}
	return nil
}

func (ft *FtSender) asyncSendLogFromDiskQueue() {
	for i := 0; i < ft.procs; i++ {
		go ft.sendFromQueue(ft.logQueue)
	}
	go ft.sendFromQueue(ft.backupQueue)
}

// trySend 从bytes反序列化数据后尝试发送数据
func (ft *FtSender) trySendBytes(dat []byte, failSleep int) (backDataContext []*datasContext, err error) {
	datas, err := ft.unmarshalData(dat)
	if err != nil {
		return
	}
	return ft.trySendDatas(datas, failSleep)
}

func ConvertDatas(ins []map[string]interface{}) []Data {
	var datas []Data
	for _, v := range ins {
		datas = append(datas, Data(v))
	}
	return datas
}
func ConvertDatasBack(ins []Data) []map[string]interface{} {
	var datas []map[string]interface{}
	for _, v := range ins {
		datas = append(datas, map[string]interface{}(v))
	}
	return datas
}

// trySendDatas 尝试发送数据，如果失败，将失败数据加入backup queue，并睡眠指定时间。返回结果为是否正常发送
func (ft *FtSender) trySendDatas(datas []Data, failSleep int) (backDataContext []*datasContext, err error) {
	err = ft.innerSender.Send(datas)
	if c, ok := err.(*utils.StatsError); ok {
		err = c.ErrorDetail
	}
	if err != nil {
		retDatasContext := ft.handleSendError(err, datas)
		for _, v := range retDatasContext {
			nnBytes, _ := json.Marshal(v)
			qErr := ft.backupQueue.Put(nnBytes)
			if qErr != nil {
				log.Errorf("Runner[%v] Sender[%v] cannot write points back to queue %v: %v", ft.runnerName, ft.innerSender.Name(), ft.backupQueue.Name(), qErr)
				backDataContext = append(backDataContext, v)
			}
		}
		time.Sleep(time.Second * time.Duration(failSleep))
	}
	return
}

func (ft *FtSender) handleSendError(err error, datas []Data) (retDatasContext []*datasContext) {
	failCtx := new(datasContext)
	var binaryUnpack bool
	se, succ := err.(*reqerr.SendError)
	if !succ {
		// 如果不是SendError 默认所有的数据都发送失败
		log.Infof("Runner[%v] Sender[%v] error type is not *SendError! reSend all datas by default", ft.runnerName, ft.innerSender.Name())
		failCtx.Datas = datas
	} else {
		failCtx.Datas = ConvertDatas(se.GetFailDatas())
		if se.ErrorType == reqerr.TypeBinaryUnpack {
			binaryUnpack = true
		}
	}
	log.Errorf("Runner[%v] Sender[%v] cannot write points: %v, failDatas size: %v", ft.runnerName, ft.innerSender.Name(), err, len(failCtx.Datas))
	log.Debugf("Runner[%v] Sender[%v] failed datas [[%v]]", ft.runnerName, ft.innerSender.Name(), failCtx.Datas)
	if binaryUnpack {
		lens := len(failCtx.Datas) / 2
		if lens > 0 {
			newFailCtx := new(datasContext)
			newFailCtx.Datas = failCtx.Datas[0:lens]
			failCtx.Datas = failCtx.Datas[lens:]
			retDatasContext = append(retDatasContext, newFailCtx)
		}
	}
	retDatasContext = append(retDatasContext, failCtx)
	return
}

func (ft *FtSender) sendFromQueue(queue queue.BackendQueue) {
	readChan := queue.ReadChan()
	timer := time.NewTicker(time.Second)
	waitCnt := 1
	var curDataContext, otherDataContext []*datasContext
	var curIdx int
	var backDataContext []*datasContext
	var err error
	for {
		if atomic.LoadInt32(&ft.stopped) > 0 {
			ft.exitChan <- struct{}{}
			return
		}
		if curIdx < len(curDataContext) {
			backDataContext, err = ft.trySendDatas(curDataContext[curIdx].Datas, waitCnt)
			curIdx++
		} else {
			select {
			case dat := <-readChan:
				backDataContext, err = ft.trySendBytes(dat, waitCnt)
			case <-timer.C:
				continue
			}
		}
		if err == nil {
			waitCnt = 1
			ft.se.AddSuccess()
		} else {
			log.Errorf("Runner[%v] Sender[%v] cannot send points from queue %v, error is %v", ft.runnerName, ft.innerSender.Name(), queue.Name(), err)
			ft.se.AddErrors()
			waitCnt++
			if waitCnt > 10 {
				waitCnt = 10
			}
		}
		if backDataContext != nil {
			otherDataContext = append(otherDataContext, backDataContext...)
		}
		if curIdx == len(curDataContext) {
			curDataContext = otherDataContext
			otherDataContext = make([]*datasContext, 0)
			curIdx = 0
		}
	}
}
