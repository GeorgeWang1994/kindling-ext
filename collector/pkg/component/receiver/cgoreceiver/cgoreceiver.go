package cgoreceiver

/*
#cgo LDFLAGS: -L ./ -lkindling  -lstdc++ -ldl
#cgo CFLAGS: -I .
#include <stdlib.h>
#include <stdint.h>
#include <stdio.h>
#include "cgo_func.h"
*/
import "C"
import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/Kindling-project/kindling/collector/pkg/component"
	analyzerpackage "github.com/Kindling-project/kindling/collector/pkg/component/analyzer"
	"github.com/Kindling-project/kindling/collector/pkg/component/receiver"
	"github.com/Kindling-project/kindling/collector/pkg/model"
)

const (
	Cgo = "cgoreceiver"
)

type CKindlingEventForGo C.struct_kindling_event_t_for_go

type CEventParamsForSubscribe C.struct_event_params_for_subscribe

type CgoReceiver struct {
	cfg             *Config
	analyzerManager *analyzerpackage.Manager
	shutdownWG      sync.WaitGroup
	telemetry       *component.TelemetryTools
	eventChannel    chan *model.KindlingEvent
	stopCh          chan interface{}
	stats           eventCounter
}

func NewCgoReceiver(config interface{}, telemetry *component.TelemetryTools, analyzerManager *analyzerpackage.Manager) receiver.Receiver {
	cfg, ok := config.(*Config)
	if !ok {
		telemetry.Logger.Panicf("Cannot convert [%s] config", Cgo)
	}
	cgoReceiver := &CgoReceiver{
		cfg:             cfg,
		analyzerManager: analyzerManager,
		telemetry:       telemetry,
		eventChannel:    make(chan *model.KindlingEvent, 3e5),
		stopCh:          make(chan interface{}, 1),
	}
	cgoReceiver.stats = newDynamicStats(cfg.SubscribeInfo)
	newSelfMetrics(telemetry.MeterProvider, cgoReceiver)
	return cgoReceiver
}

// s
func (r *CgoReceiver) Start() error {
	r.telemetry.Logger.Info("Start CgoReceiver")
	res := int(C.runForGo())
	if res == 1 {
		return fmt.Errorf("fail to init probe")
	}
	go r.getCaptureStatistics()
	go r.catchSignalUp()
	time.Sleep(2 * time.Second)
	_ = r.subEvent()
	// Wait for the C routine running
	time.Sleep(2 * time.Second)
	go r.consumeEvents()
	go r.startGetEvent()
	return nil
}

func (r *CgoReceiver) startGetEvent() {
	var pKindlingEvent unsafe.Pointer
	r.shutdownWG.Add(1)
	for {
		select {
		case <-r.stopCh:
			r.shutdownWG.Done()
			return
		default:
			res := int(C.getKindlingEvent(&pKindlingEvent))
			if res == 1 {
				event := convertEvent((*CKindlingEventForGo)(pKindlingEvent))
				r.eventChannel <- event
				r.stats.add(event.Name, 1)
			}
		}
	}
}

func (r *CgoReceiver) consumeEvents() {
	r.shutdownWG.Add(1)
	for {
		select {
		case <-r.stopCh:
			r.shutdownWG.Done()
			return
		case ev := <-r.eventChannel:
			err := r.sendToNextConsumer(ev)
			if err != nil {
				r.telemetry.Logger.Info("Failed to send KindlingEvent: ", zap.Error(err))
			}
		}
	}
}

func (r *CgoReceiver) Shutdown() error {
	// TODO stop the C routine
	close(r.stopCh)
	r.shutdownWG.Wait()
	return nil
}

func convertEvent(cgoEvent *CKindlingEventForGo) *model.KindlingEvent {
	ev := new(model.KindlingEvent)
	ev.Timestamp = uint64(cgoEvent.timestamp)
	ev.Name = C.GoString(cgoEvent.name)
	ev.Category = model.Category(cgoEvent.category)
	ev.Ctx.ThreadInfo.Pid = uint32(cgoEvent.context.tinfo.pid)
	ev.Ctx.ThreadInfo.Tid = uint32(cgoEvent.context.tinfo.tid)
	ev.Ctx.ThreadInfo.Uid = uint32(cgoEvent.context.tinfo.uid)
	ev.Ctx.ThreadInfo.Gid = uint32(cgoEvent.context.tinfo.gid)
	ev.Ctx.ThreadInfo.Comm = C.GoString(cgoEvent.context.tinfo.comm)
	ev.Ctx.ThreadInfo.ContainerId = C.GoString(cgoEvent.context.tinfo.containerId)
	ev.Ctx.FdInfo.Protocol = model.L4Proto(cgoEvent.context.fdInfo.protocol)
	ev.Ctx.FdInfo.Num = int32(cgoEvent.context.fdInfo.num)
	ev.Ctx.FdInfo.TypeFd = model.FDType(cgoEvent.context.fdInfo.fdType)
	ev.Ctx.FdInfo.Filename = C.GoString(cgoEvent.context.fdInfo.filename)
	ev.Ctx.FdInfo.Directory = C.GoString(cgoEvent.context.fdInfo.directory)
	ev.Ctx.FdInfo.Role = If(cgoEvent.context.fdInfo.role != 0, true, false).(bool)
	ev.Ctx.FdInfo.Sip = []uint32{uint32(cgoEvent.context.fdInfo.sip)}
	ev.Ctx.FdInfo.Dip = []uint32{uint32(cgoEvent.context.fdInfo.dip)}
	ev.Ctx.FdInfo.Sport = uint32(cgoEvent.context.fdInfo.sport)
	ev.Ctx.FdInfo.Dport = uint32(cgoEvent.context.fdInfo.dport)
	ev.Ctx.FdInfo.Source = uint64(cgoEvent.context.fdInfo.source)
	ev.Ctx.FdInfo.Destination = uint64(cgoEvent.context.fdInfo.destination)

	ev.ParamsNumber = uint16(cgoEvent.paramsNumber)
	ev.Latency = uint64(cgoEvent.latency)
	for i := 0; i < int(ev.ParamsNumber); i++ {
		ev.UserAttributes[i].Key = C.GoString(cgoEvent.userAttributes[i].key)
		userAttributesLen := cgoEvent.userAttributes[i].len
		ev.UserAttributes[i].Value = C.GoBytes(unsafe.Pointer(cgoEvent.userAttributes[i].value), C.int(userAttributesLen))
		ev.UserAttributes[i].ValueType = model.ValueType(cgoEvent.userAttributes[i].valueType)
	}
	return ev
}

func If(condition bool, trueVal, falseVal interface{}) interface{} {
	if condition {
		return trueVal
	}
	return falseVal
}

func (r *CgoReceiver) sendToNextConsumer(evt *model.KindlingEvent) error {
	if ce := r.telemetry.Logger.Check(zapcore.DebugLevel, ""); ce != nil {
		r.telemetry.Logger.Debug(fmt.Sprintf("Receive Event: %+v", evt))
	}
	analyzers := r.analyzerManager.GetConsumableAnalyzers(evt.Name)
	if analyzers == nil || len(analyzers) == 0 {
		//r.telemetry.Logger.Info("analyzer not found for event ", zap.String("eventName", evt.Name))
		return nil
	}
	for _, analyzer := range analyzers {
		err := analyzer.ConsumeEvent(evt)
		if err != nil {
			r.telemetry.Logger.Warn("Error sending event to next consumer: ", zap.Error(err))
		}
	}
	return nil
}

func (r *CgoReceiver) subEvent() error {
	if len(r.cfg.SubscribeInfo) == 0 {
		r.telemetry.Logger.Warn("No events are subscribed by cgoreceiver. Please check your configuration.")
	} else {
		r.telemetry.Logger.Infof("The subscribed events are: %v", r.cfg.SubscribeInfo)
	}
	for _, value := range r.cfg.SubscribeInfo {
		//to do. analyze params filed in the value
		paramsList := make([]CEventParamsForSubscribe, 0)
		var temp CEventParamsForSubscribe
		temp.name = C.CString("terminator")
		paramsList = append(paramsList, temp)
		C.subEventForGo(C.CString(value.Name), C.CString(value.Category), (unsafe.Pointer)(&paramsList[0]))
	}
	return nil
}

func (r *CgoReceiver) StartProfile() error {
	r.telemetry.Logger.Infof("Start Profile")
	if C.startProfile() == 0 {
		return nil
	} else {
		return fmt.Errorf("start Profile failed")
	}
}

func (r *CgoReceiver) StopProfile() error {
	r.telemetry.Logger.Infof("Stop Profile")
	if C.stopProfile() == 0 {
		return nil
	} else {
		return fmt.Errorf("stop Profile failed")
	}
}

func (r *CgoReceiver) ProfileModule() (submodule string, start func() error, stop func() error) {
	return "cgoreceiver", r.StartProfile, r.StopProfile
}

func (r *CgoReceiver) getCaptureStatistics() {
	C.getCaptureStatistics()
}

func (r *CgoReceiver) catchSignalUp() {
	C.catchSignalUp()
}
