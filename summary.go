package plugin_summary

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/Monibuca/engine/v3"
	. "github.com/Monibuca/utils/v3"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/mem"
	"github.com/shirou/gopsutil/net"
)

// Summary 系统摘要数据
var Summary = ServerSummary{
	control:    make(chan bool),
	reportChan: make(chan *ServerSummaryData),
}

var config = struct {
	SampleRate int
}{1}

func init() {
	plugin := &PluginConfig{
		Name:   "Summary",
		Config: &config,
		Run:    Summary.StartSummary,
	}
	InstallPlugin(plugin)
	http.HandleFunc("/api/summary", summary)
}
func summary(w http.ResponseWriter, r *http.Request) {
	CORS(w, r)
	sse := NewSSE(w, r.Context())
	Summary.Add()
	defer Summary.Done()
	summaryData, _ := getSummaryDataJson()
	_, _ = sse.Write(summaryData)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			summaryData, err := getSummaryDataJson()
			if err != nil {
				log.Println(err)
				return
			}
			if _, err := sse.Write(summaryData); err != nil {
				log.Println(err)
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func getSummaryDataJson() ([]byte, error) {
	var summaryData []byte
	var err error
	Summary.doWithDataReadLock(func() {
		summaryData, err = json.Marshal(Summary.Data)
	})
	return summaryData, err
}

// ServerSummary 控制器相关, 与具体数据分离
type ServerSummary struct {
	ref        int64
	control    chan bool
	reportChan chan *ServerSummaryData

	dataRwLock sync.RWMutex
	Data       ServerSummaryData
}

// ServerSummaryData 系统摘要定义
type ServerSummaryData struct {
	Address string
	Memory  struct {
		Total uint64
		Free  uint64
		Used  uint64
		Usage float64
	}
	CPUUsage float64
	HardDisk struct {
		Total uint64
		Free  uint64
		Used  uint64
		Usage float64
	}
	NetWork     []NetWorkInfo
	Streams     []*Stream
	lastNetWork []NetWorkInfo
	Children    map[string]*ServerSummaryData
}

// NetWorkInfo 网速信息
type NetWorkInfo struct {
	Name         string
	Receive      uint64
	Sent         uint64
	ReceiveSpeed uint64
	SentSpeed    uint64
}

// doWithDataReadLock 包装了读锁
func (s *ServerSummary) doWithDataReadLock(reader func()) {
	s.dataRwLock.RLock()
	defer s.dataRwLock.RUnlock()
	reader()
}

// doWithDataWriteLock 包装了写锁
func (s *ServerSummary) doWithDataWriteLock(writer func()) {
	s.dataRwLock.Lock()
	defer s.dataRwLock.Unlock()
	writer()
}

// StartSummary 开始定时采集数据，每秒一次
func (s *ServerSummary) StartSummary() {
	ticker := time.NewTicker(time.Second * time.Duration(config.SampleRate))
	for {
		select {
		case <-ticker.C:
			if atomic.LoadInt64(&s.ref) > 0 {
				data := Summary.collect()
				s.doWithDataWriteLock(func() {
					data.Children = s.Data.Children
					s.Data = data
				})
			}
		case v := <-s.control:
			summary := false
			s.doWithDataWriteLock(func() {
				if v {
					if atomic.AddInt64(&s.ref, 1) == 1 {
						log.Println("start report summary")
						summary = true
					}
				} else {
					if atomic.AddInt64(&s.ref, -1) == 0 {
						s.Data.lastNetWork = nil
						log.Println("stop report summary")
						summary = false
					}
				}
			})
			TriggerHook("Summary", summary)
		case report := <-s.reportChan:
			s.doWithDataWriteLock(func() {
				s.Data.Children[report.Address] = report
			})
		}
	}
}

// Running 是否正在采集数据
func (s *ServerSummary) Running() bool {
	return atomic.LoadInt64(&s.ref) > 0
}

// Add 增加订阅者
func (s *ServerSummary) Add() {
	s.control <- true
}

// Done 删除订阅者
func (s *ServerSummary) Done() {
	s.control <- false
}

// Report 上报数据
func (s *ServerSummary) Report(slave *ServerSummaryData) {
	s.reportChan <- slave
}

func (s *ServerSummary) collect() ServerSummaryData {
	v, _ := mem.VirtualMemory()
	// c, _ := cpu.Info()
	d, _ := disk.Usage("/")
	// n, _ := host.Info()
	nv, _ := net.IOCounters(true)
	// boottime, _ := host.BootTime()
	// btime := time.Unix(int64(boottime), 0).Format("2006-01-02 15:04:05")
	sd := ServerSummaryData{}
	sd.Memory.Total = v.Total / 1024 / 1024
	sd.Memory.Free = v.Available / 1024 / 1024
	sd.Memory.Used = v.Used / 1024 / 1024
	sd.Memory.Usage = v.UsedPercent
	// fmt.Printf("        Mem       : %v MB  Free: %v MB Used:%v Usage:%f%%\n", v.Total/1024/1024, v.Available/1024/1024, v.Used/1024/1024, v.UsedPercent)
	// if len(c) > 1 {
	//	for _, sub_cpu := range c {
	//		modelname := sub_cpu.ModelName
	//		cores := sub_cpu.Cores
	//		fmt.Printf("        CPU       : %v   %v cores \n", modelname, cores)
	//	}
	// } else {
	//	sub_cpu := c[0]
	//	modelname := sub_cpu.ModelName
	//	cores := sub_cpu.Cores
	//	fmt.Printf("        CPU       : %v   %v cores \n", modelname, cores)
	// }
	if cc, _ := cpu.Percent(time.Second, false); len(cc) > 0 {
		sd.CPUUsage = cc[0]
	}
	sd.HardDisk.Free = d.Free / 1024 / 1024 / 1024
	sd.HardDisk.Total = d.Total / 1024 / 1024 / 1024
	sd.HardDisk.Used = d.Used / 1024 / 1024 / 1024
	sd.HardDisk.Usage = d.UsedPercent
	sd.NetWork = make([]NetWorkInfo, len(nv))
	for i, n := range nv {
		sd.NetWork[i].Name = n.Name
		sd.NetWork[i].Receive = n.BytesRecv
		sd.NetWork[i].Sent = n.BytesSent
		if sd.lastNetWork != nil && len(sd.lastNetWork) > i {
			sd.NetWork[i].ReceiveSpeed = n.BytesRecv - sd.lastNetWork[i].Receive
			sd.NetWork[i].SentSpeed = n.BytesSent - sd.lastNetWork[i].Sent
		}
	}
	sd.lastNetWork = sd.NetWork
	// fmt.Printf("        Network: %v bytes / %v bytes\n", nv[0].BytesRecv, nv[0].BytesSent)
	// fmt.Printf("        SystemBoot:%v\n", btime)
	// fmt.Printf("        CPU Used    : used %f%% \n", cc[0])
	// fmt.Printf("        HD        : %v GB  Free: %v GB Usage:%f%%\n", d.Total/1024/1024/1024, d.Free/1024/1024/1024, d.UsedPercent)
	// fmt.Printf("        OS        : %v(%v)   %v  \n", n.Platform, n.PlatformFamily, n.PlatformVersion)
	// fmt.Printf("        Hostname  : %v  \n", n.Hostname)
	sd.Streams = Streams.ToList()
	return sd
}
