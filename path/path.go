package path

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aler9/gortsplib"
	"github.com/aler9/gortsplib/base"

	"github.com/aler9/rtsp-simple-server/client"
	"github.com/aler9/rtsp-simple-server/conf"
	"github.com/aler9/rtsp-simple-server/externalcmd"
	"github.com/aler9/rtsp-simple-server/sourcertmp"
	"github.com/aler9/rtsp-simple-server/sourcertsp"
	"github.com/aler9/rtsp-simple-server/stats"
)

func newEmptyTimer() *time.Timer {
	t := time.NewTimer(0)
	<-t.C
	return t
}

type Parent interface {
	Log(string, ...interface{})
	OnPathClose(*Path)
	OnPathClientClose(*client.Client)
}

// a source can be
// * client.Client
// * sourcertsp.Source
// * sourcertmp.Source
// * sourceRedirect
type source interface {
	IsSource()
}

// a sourceExternal can be
// * sourcertsp.Source
// * sourcertmp.Source
type sourceExternal interface {
	IsSource()
	IsSourceExternal()
	Close()
}

type sourceRedirect struct{}

func (*sourceRedirect) IsSource() {}

type ClientDescribeRes struct {
	Path client.Path
	Err  error
}

type ClientDescribeReq struct {
	Res      chan ClientDescribeRes
	Client   *client.Client
	PathName string
	Req      *base.Request
}

type ClientAnnounceRes struct {
	Path client.Path
	Err  error
}

type ClientAnnounceReq struct {
	Res      chan ClientAnnounceRes
	Client   *client.Client
	PathName string
	Tracks   gortsplib.Tracks
	Req      *base.Request
}

type ClientSetupPlayRes struct {
	Path client.Path
	Err  error
}

type ClientSetupPlayReq struct {
	Res      chan ClientSetupPlayRes
	Client   *client.Client
	PathName string
	TrackId  int
	Req      *base.Request
}

type clientRemoveReq struct {
	res    chan struct{}
	client *client.Client
}

type clientPlayReq struct {
	res    chan struct{}
	client *client.Client
}

type clientRecordReq struct {
	res    chan struct{}
	client *client.Client
}

type clientState int

const (
	clientStateWaitingDescribe clientState = iota
	clientStatePrePlay
	clientStatePlay
	clientStatePreRecord
	clientStateRecord
	clientStatePreRemove
)

type sourceState int

const (
	sourceStateNotReady sourceState = iota
	sourceStateWaitingDescribe
	sourceStateReady
)

type Path struct {
	readTimeout  time.Duration
	writeTimeout time.Duration
	confName     string
	conf         *conf.PathConf
	name         string
	wg           *sync.WaitGroup
	stats        *stats.Stats
	parent       Parent

	clients                      map[*client.Client]clientState
	clientsWg                    sync.WaitGroup
	source                       source
	sourceTrackCount             int
	sourceSdp                    []byte
	readers                      *readersMap
	onInitCmd                    *externalcmd.ExternalCmd
	onDemandCmd                  *externalcmd.ExternalCmd
	describeTimer                *time.Timer
	sourceCloseTimer             *time.Timer
	sourceCloseTimerStarted      bool
	sourceState                  sourceState
	sourceWg                     sync.WaitGroup
	runOnDemandCloseTimer        *time.Timer
	runOnDemandCloseTimerStarted bool
	closeTimer                   *time.Timer
	closeTimerStarted            bool

	// in
	sourceSetReady    chan struct{}           // from source
	sourceSetNotReady chan struct{}           // from source
	clientDescribe    chan ClientDescribeReq  // from program
	clientAnnounce    chan ClientAnnounceReq  // from program
	clientSetupPlay   chan ClientSetupPlayReq // from program
	clientPlay        chan clientPlayReq      // from client
	clientRecord      chan clientRecordReq    // from client
	clientRemove      chan clientRemoveReq    // from client
	terminate         chan struct{}
}

func New(
	readTimeout time.Duration,
	writeTimeout time.Duration,
	confName string,
	conf *conf.PathConf,
	name string,
	wg *sync.WaitGroup,
	stats *stats.Stats,
	parent Parent) *Path {

	pa := &Path{
		readTimeout:           readTimeout,
		writeTimeout:          writeTimeout,
		confName:              confName,
		conf:                  conf,
		name:                  name,
		wg:                    wg,
		stats:                 stats,
		parent:                parent,
		clients:               make(map[*client.Client]clientState),
		readers:               newReadersMap(),
		describeTimer:         newEmptyTimer(),
		sourceCloseTimer:      newEmptyTimer(),
		runOnDemandCloseTimer: newEmptyTimer(),
		closeTimer:            newEmptyTimer(),
		sourceSetReady:        make(chan struct{}),
		sourceSetNotReady:     make(chan struct{}),
		clientDescribe:        make(chan ClientDescribeReq),
		clientAnnounce:        make(chan ClientAnnounceReq),
		clientSetupPlay:       make(chan ClientSetupPlayReq),
		clientPlay:            make(chan clientPlayReq),
		clientRecord:          make(chan clientRecordReq),
		clientRemove:          make(chan clientRemoveReq),
		terminate:             make(chan struct{}),
	}

	pa.wg.Add(1)
	go pa.run()
	return pa
}

func (pa *Path) Close() {
	close(pa.terminate)
}

func (pa *Path) Log(format string, args ...interface{}) {
	pa.parent.Log("[path "+pa.name+"] "+format, args...)
}

func (pa *Path) run() {
	defer pa.wg.Done()

	if pa.conf.Source == "redirect" {
		pa.source = &sourceRedirect{}

	} else if pa.hasExternalSource() && !pa.conf.SourceOnDemand {
		pa.startExternalSource()
	}

	if pa.conf.RunOnInit != "" {
		pa.Log("on init command started")
		pa.onInitCmd = externalcmd.New(pa.conf.RunOnInit,
			pa.conf.RunOnInitRestart, pa.name)
	}

outer:
	for {
		select {
		case <-pa.describeTimer.C:
			for c, state := range pa.clients {
				if state == clientStateWaitingDescribe {
					pa.removeClient(c)
					c.OnPathDescribeData(nil, "", fmt.Errorf("publisher of path '%s' has timed out", pa.name))
				}
			}

			// set state after removeClient(), so schedule* works once
			pa.sourceState = sourceStateNotReady

			pa.scheduleSourceClose()
			pa.scheduleRunOnDemandClose()
			pa.scheduleClose()

		case <-pa.sourceCloseTimer.C:
			pa.sourceCloseTimerStarted = false
			pa.source.(sourceExternal).Close()
			pa.source = nil

			pa.scheduleClose()

		case <-pa.runOnDemandCloseTimer.C:
			pa.runOnDemandCloseTimerStarted = false
			pa.Log("on demand command stopped")
			pa.onDemandCmd.Close()
			pa.onDemandCmd = nil

			pa.scheduleClose()

		case <-pa.closeTimer.C:
			pa.exhaustChannels()
			pa.parent.OnPathClose(pa)
			<-pa.terminate
			break outer

		case <-pa.sourceSetReady:
			pa.onSourceSetReady()

		case <-pa.sourceSetNotReady:
			pa.onSourceSetNotReady()

		case req := <-pa.clientDescribe:
			if _, ok := pa.clients[req.Client]; ok {
				req.Res <- ClientDescribeRes{nil, fmt.Errorf("already subscribed")}
				continue
			}

			// reply immediately
			req.Res <- ClientDescribeRes{pa, nil}

			pa.onClientDescribe(req.Client)

		case req := <-pa.clientSetupPlay:
			err := pa.onClientSetupPlay(req.Client, req.TrackId)
			if err != nil {
				req.Res <- ClientSetupPlayRes{nil, err}
				continue
			}
			req.Res <- ClientSetupPlayRes{pa, nil}

		case req := <-pa.clientPlay:
			pa.onClientPlay(req.client)
			close(req.res)

		case req := <-pa.clientAnnounce:
			err := pa.onClientAnnounce(req.Client, req.Tracks)
			if err != nil {
				req.Res <- ClientAnnounceRes{nil, err}
				continue
			}
			req.Res <- ClientAnnounceRes{pa, nil}

		case req := <-pa.clientRecord:
			pa.onClientRecord(req.client)
			close(req.res)

		case req := <-pa.clientRemove:
			if _, ok := pa.clients[req.client]; !ok {
				close(req.res)
				continue
			}

			if pa.clients[req.client] != clientStatePreRemove {
				pa.removeClient(req.client)
			}

			delete(pa.clients, req.client)
			pa.clientsWg.Done()

			close(req.res)

		case <-pa.terminate:
			pa.exhaustChannels()
			break outer
		}
	}

	pa.describeTimer.Stop()
	pa.sourceCloseTimer.Stop()
	pa.runOnDemandCloseTimer.Stop()
	pa.closeTimer.Stop()

	if pa.onInitCmd != nil {
		pa.Log("on init command stopped")
		pa.onInitCmd.Close()
	}

	if source, ok := pa.source.(sourceExternal); ok {
		source.Close()
	}
	pa.sourceWg.Wait()

	if pa.onDemandCmd != nil {
		pa.Log("on demand command stopped")
		pa.onDemandCmd.Close()
	}

	for c, state := range pa.clients {
		if state != clientStatePreRemove {
			switch state {
			case clientStatePlay:
				atomic.AddInt64(pa.stats.CountReaders, -1)
				pa.readers.remove(c)

			case clientStateRecord:
				atomic.AddInt64(pa.stats.CountPublishers, -1)
			}
			pa.parent.OnPathClientClose(c)
		}
	}
	pa.clientsWg.Wait()

	close(pa.sourceSetReady)
	close(pa.sourceSetNotReady)
	close(pa.clientDescribe)
	close(pa.clientAnnounce)
	close(pa.clientSetupPlay)
	close(pa.clientPlay)
	close(pa.clientRecord)
	close(pa.clientRemove)
}

func (pa *Path) exhaustChannels() {
	go func() {
		for {
			select {
			case _, ok := <-pa.sourceSetReady:
				if !ok {
					return
				}

			case _, ok := <-pa.sourceSetNotReady:
				if !ok {
					return
				}

			case req, ok := <-pa.clientDescribe:
				if !ok {
					return
				}
				req.Res <- ClientDescribeRes{nil, fmt.Errorf("terminated")}

			case req, ok := <-pa.clientAnnounce:
				if !ok {
					return
				}
				req.Res <- ClientAnnounceRes{nil, fmt.Errorf("terminated")}

			case req, ok := <-pa.clientSetupPlay:
				if !ok {
					return
				}
				req.Res <- ClientSetupPlayRes{nil, fmt.Errorf("terminated")}

			case req, ok := <-pa.clientPlay:
				if !ok {
					return
				}
				close(req.res)

			case req, ok := <-pa.clientRecord:
				if !ok {
					return
				}
				close(req.res)

			case req, ok := <-pa.clientRemove:
				if !ok {
					return
				}

				if _, ok := pa.clients[req.client]; !ok {
					close(req.res)
					continue
				}

				pa.clientsWg.Done()

				close(req.res)
			}
		}
	}()
}

func (pa *Path) hasExternalSource() bool {
	return strings.HasPrefix(pa.conf.Source, "rtsp://") ||
		strings.HasPrefix(pa.conf.Source, "rtmp://")
}

func (pa *Path) startExternalSource() {
	if strings.HasPrefix(pa.conf.Source, "rtsp://") {
		pa.source = sourcertsp.New(pa.conf.Source, pa.conf.SourceProtocolParsed,
			pa.readTimeout, pa.writeTimeout, &pa.sourceWg, pa.stats, pa)

	} else if strings.HasPrefix(pa.conf.Source, "rtmp://") {
		pa.source = sourcertmp.New(pa.conf.Source, &pa.sourceWg, pa.stats, pa)
	}
}

func (pa *Path) hasClients() bool {
	for _, state := range pa.clients {
		if state != clientStatePreRemove {
			return true
		}
	}
	return false
}

func (pa *Path) hasClientsNotSources() bool {
	for c, state := range pa.clients {
		if state != clientStatePreRemove && c != pa.source {
			return true
		}
	}
	return false
}

func (pa *Path) addClient(c *client.Client, state clientState) {
	if _, ok := pa.clients[c]; ok {
		panic("client already added")
	}

	pa.clients[c] = state
	pa.clientsWg.Add(1)
}

func (pa *Path) removeClient(c *client.Client) {
	state := pa.clients[c]
	pa.clients[c] = clientStatePreRemove

	switch state {
	case clientStatePlay:
		atomic.AddInt64(pa.stats.CountReaders, -1)
		pa.readers.remove(c)

	case clientStateRecord:
		atomic.AddInt64(pa.stats.CountPublishers, -1)
		pa.onSourceSetNotReady()
	}

	if pa.source == c {
		pa.source = nil

		// close all clients that are reading or waiting to read
		for oc, state := range pa.clients {
			if state != clientStatePreRemove && state != clientStateWaitingDescribe {
				pa.removeClient(oc)
				pa.parent.OnPathClientClose(oc)
			}
		}
	}

	pa.scheduleSourceClose()
	pa.scheduleRunOnDemandClose()
	pa.scheduleClose()
}

func (pa *Path) onSourceSetReady() {
	if pa.sourceState == sourceStateWaitingDescribe {
		pa.describeTimer.Stop()
		pa.describeTimer = newEmptyTimer()
	}

	pa.sourceState = sourceStateReady

	// reply to all clients that are waiting for a description
	for c, state := range pa.clients {
		if state == clientStateWaitingDescribe {
			pa.removeClient(c)
			c.OnPathDescribeData(pa.sourceSdp, "", nil)
		}
	}

	pa.scheduleSourceClose()
	pa.scheduleRunOnDemandClose()
	pa.scheduleClose()
}

func (pa *Path) onSourceSetNotReady() {
	pa.sourceState = sourceStateNotReady

	// close all clients that are reading or waiting to read
	for c, state := range pa.clients {
		if state == clientStateWaitingDescribe {
			panic("not possible")
		}
		if c != pa.source && state != clientStatePreRemove {
			pa.removeClient(c)
			pa.parent.OnPathClientClose(c)
		}
	}
}

func (pa *Path) onClientDescribe(c *client.Client) {
	// prevent on-demand source from closing
	if pa.sourceCloseTimerStarted {
		pa.sourceCloseTimer = newEmptyTimer()
		pa.sourceCloseTimerStarted = false
	}

	// prevent on-demand command from closing
	if pa.runOnDemandCloseTimerStarted {
		pa.runOnDemandCloseTimer = newEmptyTimer()
		pa.runOnDemandCloseTimerStarted = false
	}

	// start on-demand source
	if pa.hasExternalSource() {
		if pa.source == nil {
			pa.startExternalSource()

			if pa.sourceState != sourceStateWaitingDescribe {
				pa.describeTimer = time.NewTimer(pa.conf.SourceOnDemandStartTimeout)
				pa.sourceState = sourceStateWaitingDescribe
			}
		}
	}

	// start on-demand command
	if pa.conf.RunOnDemand != "" {
		if pa.onDemandCmd == nil {
			pa.Log("on demand command started")
			pa.onDemandCmd = externalcmd.New(pa.conf.RunOnDemand,
				pa.conf.RunOnDemandRestart, pa.name)

			if pa.sourceState != sourceStateWaitingDescribe {
				pa.describeTimer = time.NewTimer(pa.conf.RunOnDemandStartTimeout)
				pa.sourceState = sourceStateWaitingDescribe
			}
		}
	}

	if _, ok := pa.source.(*sourceRedirect); ok {
		pa.addClient(c, clientStatePreRemove)
		pa.removeClient(c)
		c.OnPathDescribeData(nil, pa.conf.SourceRedirect, nil)
		return
	}

	switch pa.sourceState {
	case sourceStateReady:
		pa.addClient(c, clientStatePreRemove)
		pa.removeClient(c)
		c.OnPathDescribeData(pa.sourceSdp, "", nil)
		return

	case sourceStateWaitingDescribe:
		pa.addClient(c, clientStateWaitingDescribe)
		return

	case sourceStateNotReady:
		pa.addClient(c, clientStatePreRemove)
		pa.removeClient(c)
		c.OnPathDescribeData(nil, "", fmt.Errorf("no one is publishing to path '%s'", pa.name))
		return
	}
}

func (pa *Path) onClientSetupPlay(c *client.Client, trackId int) error {
	if pa.sourceState != sourceStateReady {
		return fmt.Errorf("no one is publishing to path '%s'", pa.name)
	}

	if trackId >= pa.sourceTrackCount {
		return fmt.Errorf("track %d does not exist", trackId)
	}

	if _, ok := pa.clients[c]; !ok {
		// prevent on-demand source from closing
		if pa.sourceCloseTimerStarted {
			pa.sourceCloseTimer = newEmptyTimer()
			pa.sourceCloseTimerStarted = false
		}

		// prevent on-demand command from closing
		if pa.runOnDemandCloseTimerStarted {
			pa.runOnDemandCloseTimer = newEmptyTimer()
			pa.runOnDemandCloseTimerStarted = false
		}

		pa.addClient(c, clientStatePrePlay)
	}

	return nil
}

func (pa *Path) onClientPlay(c *client.Client) {
	state, ok := pa.clients[c]
	if !ok {
		return
	}

	if state != clientStatePrePlay {
		return
	}

	atomic.AddInt64(pa.stats.CountReaders, 1)
	pa.clients[c] = clientStatePlay
	pa.readers.add(c)
}

func (pa *Path) onClientAnnounce(c *client.Client, tracks gortsplib.Tracks) error {
	if _, ok := pa.clients[c]; ok {
		return fmt.Errorf("already subscribed")
	}

	if pa.source != nil || pa.hasExternalSource() {
		return fmt.Errorf("someone is already publishing to path '%s'", pa.name)
	}

	pa.addClient(c, clientStatePreRecord)

	pa.source = c
	pa.sourceTrackCount = len(tracks)
	pa.sourceSdp = tracks.Write()
	return nil
}

func (pa *Path) onClientRecord(c *client.Client) {
	state, ok := pa.clients[c]
	if !ok {
		return
	}

	if state != clientStatePreRecord {
		return
	}

	atomic.AddInt64(pa.stats.CountPublishers, 1)
	pa.clients[c] = clientStateRecord

	pa.onSourceSetReady()
}

func (pa *Path) scheduleSourceClose() {
	if !pa.hasExternalSource() || !pa.conf.SourceOnDemand || pa.source == nil {
		return
	}

	if pa.sourceCloseTimerStarted ||
		pa.sourceState == sourceStateWaitingDescribe ||
		pa.hasClients() {
		return
	}

	pa.sourceCloseTimer.Stop()
	pa.sourceCloseTimer = time.NewTimer(pa.conf.SourceOnDemandCloseAfter)
	pa.sourceCloseTimerStarted = true
}

func (pa *Path) scheduleRunOnDemandClose() {
	if pa.conf.RunOnDemand == "" || pa.onDemandCmd == nil {
		return
	}

	if pa.runOnDemandCloseTimerStarted ||
		pa.sourceState == sourceStateWaitingDescribe ||
		pa.hasClientsNotSources() {
		return
	}

	pa.runOnDemandCloseTimer.Stop()
	pa.runOnDemandCloseTimer = time.NewTimer(pa.conf.RunOnDemandCloseAfter)
	pa.runOnDemandCloseTimerStarted = true
}

func (pa *Path) scheduleClose() {
	if pa.closeTimerStarted ||
		pa.conf.Regexp == nil ||
		pa.hasClients() ||
		pa.source != nil {
		return
	}

	pa.closeTimer.Stop()
	pa.closeTimer = time.NewTimer(0)
	pa.closeTimerStarted = true
}

func (pa *Path) OnSourceSetReady(tracks gortsplib.Tracks) {
	pa.sourceSdp = tracks.Write()
	pa.sourceTrackCount = len(tracks)
	pa.sourceSetReady <- struct{}{}
}

func (pa *Path) OnSourceSetNotReady() {
	pa.sourceSetNotReady <- struct{}{}
}

func (pa *Path) ConfName() string {
	return pa.confName
}

func (pa *Path) Conf() *conf.PathConf {
	return pa.conf
}

func (pa *Path) Name() string {
	return pa.name
}

func (pa *Path) SourceTrackCount() int {
	return pa.sourceTrackCount
}

func (pa *Path) OnPathManDescribe(req ClientDescribeReq) {
	pa.clientDescribe <- req
}

func (pa *Path) OnPathManSetupPlay(req ClientSetupPlayReq) {
	pa.clientSetupPlay <- req
}

func (pa *Path) OnPathManAnnounce(req ClientAnnounceReq) {
	pa.clientAnnounce <- req
}

func (pa *Path) OnClientRemove(c *client.Client) {
	res := make(chan struct{})
	pa.clientRemove <- clientRemoveReq{res, c}
	<-res
}

func (pa *Path) OnClientPlay(c *client.Client) {
	res := make(chan struct{})
	pa.clientPlay <- clientPlayReq{res, c}
	<-res
}

func (pa *Path) OnClientRecord(c *client.Client) {
	res := make(chan struct{})
	pa.clientRecord <- clientRecordReq{res, c}
	<-res
}

func (pa *Path) OnFrame(trackId int, streamType gortsplib.StreamType, buf []byte) {
	pa.readers.forwardFrame(trackId, streamType, buf)
}