package server

import (
  "io"
  "net"
  "sync"
  "time"

  "github.com/juju/errors"
  "github.com/pion/rtcp"
  "github.com/pion/rtp"
  "github.com/pion/webrtc/v3"
)

type TrackInfo struct {
  SSRC     uint32
  ID       string
  StreamID string
  Kind     webrtc.RTPCodecType
  Mid      string
}

type TrackEventType uint8

const (
  TrackEventTypeAdd TrackEventType = iota + 1
  TrackEventTypeRemove
)

type TrackEvent struct {
  TrackInfo
  Type TrackEventType
}

type WebRTCTransportFactory struct {
  loggerFactory LoggerFactory
  iceServers    []ICEServer
  webrtcAPI     *webrtc.API
}

func NewWebRTCTransportFactory(
  loggerFactory LoggerFactory,
  iceServers []ICEServer,
  sfuConfig NetworkConfigSFU,
) *WebRTCTransportFactory {
  allowedInterfaces := map[string]struct{}{}
  for _, iface := range sfuConfig.Interfaces {
    allowedInterfaces[iface] = struct{}{}
  }

  log := loggerFactory.GetLogger("webrtctransport")

  settingEngine := webrtc.SettingEngine{
    LoggerFactory: NewPionLoggerFactory(loggerFactory),
  }

  settingEngine.SetSDPMediaLevelFingerprints(true)

  networkTypes := NewNetworkTypes(loggerFactory.GetLogger("networktype"), sfuConfig.Protocols)
  settingEngine.SetNetworkTypes(networkTypes)

  if udp := sfuConfig.UDP; udp.PortMin > 0 && udp.PortMax > 0 {
    if err := settingEngine.SetEphemeralUDPPortRange(udp.PortMin, udp.PortMax); err != nil {
      err = errors.Trace(err)
      log.Printf("Error setting epheremal UDP port range (%d-%d): %s", udp.PortMin, udp.PortMin, err)
    } else {
      log.Printf("Set epheremal UDP port range to %d-%d", udp.PortMin, udp.PortMax)
    }
  }

  tcpEnabled := false

  for _, networkType := range networkTypes {
    if networkType == webrtc.NetworkTypeTCP4 || networkType == webrtc.NetworkTypeTCP6 {
      tcpEnabled = true

      break
    }
  }

  if tcpEnabled {
    tcpListener, err := net.ListenTCP("tcp", &net.TCPAddr{
      IP:   net.ParseIP(sfuConfig.TCPBindAddr),
      Port: sfuConfig.TCPListenPort,
      Zone: "",
    })
    if err != nil {
      log.Printf("Error starting TCP listener: %+v", errors.Trace(err))
    } else {
      logger := settingEngine.LoggerFactory.NewLogger("ice-tcp")
      log.Printf("ICE TCP listener started on %s", tcpListener.Addr())
      settingEngine.SetICETCPMux(webrtc.NewICETCPMux(logger, tcpListener, 32))
    }
  }

  if len(allowedInterfaces) > 0 {
    settingEngine.SetInterfaceFilter(func(iface string) bool {
      _, ok := allowedInterfaces[iface]

      return ok
    })
  }

  var mediaEngine webrtc.MediaEngine

  RegisterCodecs(&mediaEngine, sfuConfig.JitterBuffer)

  api := webrtc.NewAPI(
    webrtc.WithMediaEngine(&mediaEngine),
    webrtc.WithSettingEngine(settingEngine),
  )

  return &WebRTCTransportFactory{loggerFactory, iceServers, api}
}

func RegisterCodecs(mediaEngine *webrtc.MediaEngine, jitterBufferEnabled bool) {
  rtcpfb := []webrtc.RTCPFeedback{
    {
      Type: webrtc.TypeRTCPFBGoogREMB,
    },
    // webrtc.RTCPFeedback{
    // 	Type:      webrtc.TypeRTCPFBCCM,
    // 	Parameter: "fir",
    // },

    // https://tools.ietf.org/html/rfc4585#section-4.2
    // "pli" indicates the use of Picture Loss Indication feedback as defined
    // in Section 6.3.1.
    {
      Type:      webrtc.TypeRTCPFBNACK,
      Parameter: "pli",
    },
  }

  if jitterBufferEnabled {
    // The feedback type "nack", without parameters, indicates use of the
    // Generic NACK feedback format as defined in Section 6.2.1.
    rtcpfb = append(rtcpfb, webrtc.RTCPFeedback{
      Type:      webrtc.TypeRTCPFBNACK,
      Parameter: "",
    })
  }

  // s.mediaEngine.RegisterCodec(webrtc.NewRTPH264CodecExt(webrtc.DefaultPayloadTypeH264, 90000, rtcpfb, IOSH264Fmtp))
  // s.mediaEngine.RegisterCodec(webrtc.NewRTPVP9Codec(webrtc.DefaultPayloadTypeVP9, 90000))
  if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
    RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "video/VP8", ClockRate: 90000, Channels: 0, SDPFmtpLine: "", RTCPFeedback: rtcpfb},
    PayloadType:        96,
  }, webrtc.RTPCodecTypeVideo); err != nil {
    panic(err)
  }
  if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
    RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "audio/opus", ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1", RTCPFeedback: nil},
    PayloadType:        111,
  }, webrtc.RTPCodecTypeAudio); err != nil {
    panic(err)
  }
}

type WebRTCTransport struct {
  mu sync.RWMutex
  wg sync.WaitGroup

  log     Logger
  rtpLog  Logger
  rtcpLog Logger

  clientID        string
  peerConnection  *webrtc.PeerConnection
  signaller       *Signaller
  dataTransceiver *DataTransceiver

  trackEventsCh chan TrackEvent
  rtpCh         chan *rtp.Packet
  rtcpCh        chan rtcp.Packet

  localTracks  map[uint32]localTrackInfo
  remoteTracks map[uint32]remoteTrackInfo
}

var _ Transport = &WebRTCTransport{}

func (f WebRTCTransportFactory) NewWebRTCTransport(clientID string) (*WebRTCTransport, error) {
  webrtcICEServers := []webrtc.ICEServer{}

  for _, iceServer := range GetICEAuthServers(f.iceServers) {
    var c webrtc.ICECredentialType
    if iceServer.Username != "" && iceServer.Credential != "" {
      c = webrtc.ICECredentialTypePassword
    }

    webrtcICEServers = append(webrtcICEServers, webrtc.ICEServer{
      URLs:           iceServer.URLs,
      CredentialType: c,
      Username:       iceServer.Username,
      Credential:     iceServer.Credential,
    })
  }

  webrtcConfig := webrtc.Configuration{
    ICEServers: webrtcICEServers,
  }

  peerConnection, err := f.webrtcAPI.NewPeerConnection(webrtcConfig)
  if err != nil {
    return nil, errors.Annotate(err, "new peer connection")
  }

  return NewWebRTCTransport(f.loggerFactory, clientID, true, peerConnection)
}

func NewWebRTCTransport(
  loggerFactory LoggerFactory, clientID string, initiator bool, peerConnection *webrtc.PeerConnection,
) (*WebRTCTransport, error) {
  closePeer := func(reason error) error {
    var errs MultiErrorHandler

    errs.Add(reason)

    err := peerConnection.Close()
    if err != nil {
      errs.Add(errors.Annotatef(err, "close peer connection"))
    }

    return errors.Trace(errs.Err())
  }

  var (
    dataChannel *webrtc.DataChannel
    err         error
  )

  if initiator {
    // need to do this to connect with simple peer
    // only when we are the initiator
    dataChannel, err = peerConnection.CreateDataChannel("data", nil)
    if err != nil {
      return nil, closePeer(errors.Annotate(err, "create data channel"))
    }
  }

  dataTransceiver := NewDataTransceiver(loggerFactory, clientID, dataChannel, peerConnection)

  signaller, err := NewSignaller(
    loggerFactory,
    initiator,
    peerConnection,
    localPeerID,
    clientID,
  )

  log := loggerFactory.GetLogger("webrtctransport")

  peerConnection.OnICEGatheringStateChange(func(state webrtc.ICEGathererState) {
    log.Printf("[%s] ICE gathering state changed: %s", clientID, state)
  })

  if err != nil {
    return nil, closePeer(errors.Annotate(err, "initialize signaller"))
  }

  rtpLog := loggerFactory.GetLogger("rtp")
  rtcpLog := loggerFactory.GetLogger("rtcp")

  transport := &WebRTCTransport{
    log:     log,
    rtpLog:  rtpLog,
    rtcpLog: rtcpLog,

    clientID:        clientID,
    signaller:       signaller,
    peerConnection:  peerConnection,
    dataTransceiver: dataTransceiver,

    trackEventsCh: make(chan TrackEvent),
    rtpCh:         make(chan *rtp.Packet),
    rtcpCh:        make(chan rtcp.Packet),

    localTracks:  map[uint32]localTrackInfo{},
    remoteTracks: map[uint32]remoteTrackInfo{},
  }
  peerConnection.OnTrack(transport.handleTrack)

  go func() {
    // wait for peer connection to be closed
    <-signaller.CloseChannel()
    // do not close channels before all writing goroutines exit
    transport.wg.Wait()
    transport.dataTransceiver.Close()
    close(transport.rtpCh)
    close(transport.rtcpCh)
    close(transport.trackEventsCh)
  }()
  return transport, nil
}

type localTrackInfo struct {
  trackInfo   TrackInfo
  transceiver *webrtc.RTPTransceiver
  sender      *webrtc.RTPSender
  track       *webrtc.TrackLocalStaticRTP
}

type remoteTrackInfo struct {
  trackInfo   TrackInfo
  transceiver *webrtc.RTPTransceiver
  receiver    *webrtc.RTPReceiver
  track       *webrtc.TrackRemote
}

func (p *WebRTCTransport) Close() error {
  return p.signaller.Close()
}

func (p *WebRTCTransport) ClientID() string {
  return p.clientID
}

func (p *WebRTCTransport) WriteRTCP(packets []rtcp.Packet) error {
  p.rtcpLog.Printf("[%s] WriteRTCP: %s", p.clientID, packets)

  err := p.peerConnection.WriteRTCP(packets)
  if err == nil {
    prometheusRTCPPacketsSent.Inc()
  }

  return errors.Annotate(err, "write rtcp")
}

func (p *WebRTCTransport) CloseChannel() <-chan struct{} {
  return p.signaller.CloseChannel()
}

func (p *WebRTCTransport) WriteRTP(packet *rtp.Packet) (bytes int, err error) {
  p.rtpLog.Printf("[%s] WriteRTP: %s", p.clientID, packet)

  p.mu.RLock()
  pta, ok := p.localTracks[packet.SSRC]
  p.mu.RUnlock()

  if !ok {
    return 0, errors.Errorf("track %d not found", packet.SSRC)
  }

  err = pta.track.WriteRTP(packet)
  if errIs(err, io.ErrClosedPipe) {
    // ErrClosedPipe means we don't have any subscribers, this is ok if no peers have connected yet
    return 0, nil
  }

  if err != nil {
    return 0, errors.Annotate(err, "write rtp")
  }

  prometheusRTPPacketsSent.Inc()
  prometheusRTPPacketsSentBytes.Add(float64(packet.MarshalSize()))

  return packet.MarshalSize(), nil
}

func (p *WebRTCTransport) RemoveTrack(ssrc uint32) error {
  p.mu.Lock()
  pta, ok := p.localTracks[ssrc]
  if ok {
    delete(p.localTracks, ssrc)
  }
  p.mu.Unlock()

  if !ok {
    return errors.Errorf("track %d not found", ssrc)
  }

  err := p.peerConnection.RemoveTrack(pta.sender)
  if err != nil {
    return errors.Annotate(err, "remove track")
  }

  p.signaller.Negotiate()

  return nil
}

func (p *WebRTCTransport) AddTrack(ssrc uint32, id string, streamId string) error {
  track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, id, streamId)
  if err != nil {
    return errors.Annotate(err, "new track")
  }

  sender, err := p.peerConnection.AddTrack(track)
  if err != nil {
    return errors.Annotate(err, "add track")
  }

  if p.signaller.Initiator() {
    p.signaller.Negotiate()
  } else {
    p.signaller.SendTransceiverRequest(track.Kind(), webrtc.RTPTransceiverDirectionRecvonly)
  }

  p.wg.Add(1)

  go func() {
    defer p.wg.Done()

    for {
      rtcpPackets, _, err := sender.ReadRTCP()
      if err != nil {
        return
      }

      for _, rtcpPacket := range rtcpPackets {
        p.rtcpLog.Printf("[%s] ReadRTCP: %s", p.clientID, rtcpPacket)
        prometheusRTCPPacketsReceived.Inc()
        p.rtcpCh <- rtcpPacket
      }
    }
  }()

  var transceiver *webrtc.RTPTransceiver

  for _, tr := range p.peerConnection.GetTransceivers() {
    if tr.Sender() == sender {
      transceiver = tr

      break
    }
  }

  trackInfo := TrackInfo{
    SSRC:     ssrc,
    ID:       track.ID(),
    StreamID: track.StreamID(),
    Kind:     track.Kind(),
    Mid:      "",
  }

  p.mu.Lock()
  p.localTracks[ssrc] = localTrackInfo{trackInfo, transceiver, sender, track}
  p.mu.Unlock()

  return nil
}

func (p *WebRTCTransport) addRemoteTrack(rti remoteTrackInfo) {
  p.mu.Lock()
  defer p.mu.Unlock()

  p.remoteTracks[rti.trackInfo.SSRC] = rti
}

func (p *WebRTCTransport) removeRemoteTrack(ssrc uint32) {
  p.mu.Lock()
  defer p.mu.Unlock()

  delete(p.remoteTracks, ssrc)
}

// RemoteTracks returns info about receiving tracks
func (p *WebRTCTransport) RemoteTracks() []TrackInfo {
  p.mu.Lock()
  defer p.mu.Unlock()

  list := make([]TrackInfo, 0, len(p.remoteTracks))

  for _, rti := range p.remoteTracks {
    trackInfo := rti.trackInfo
    trackInfo.Mid = rti.transceiver.Mid()
    list = append(list, trackInfo)
  }

  return list
}

// LocalTracks returns info about sending tracks
func (p *WebRTCTransport) LocalTracks() []TrackInfo {
  p.mu.Lock()
  defer p.mu.Unlock()

  list := make([]TrackInfo, 0, len(p.localTracks))

  for _, lti := range p.localTracks {
    trackInfo := lti.trackInfo
    trackInfo.Mid = lti.transceiver.Mid()
    list = append(list, trackInfo)
  }

  return list
}

func (p *WebRTCTransport) handleTrack(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
  trackInfo := TrackInfo{
    SSRC:     uint32(track.SSRC()),
    ID:       track.ID(),
    StreamID: track.StreamID(),
    Kind:     track.Kind(),
    Mid:      "",
  }

  p.log.Printf("[%s] Remote track: %d", p.clientID, trackInfo.SSRC)

  start := time.Now()

  prometheusWebRTCTracksTotal.Inc()
  prometheusWebRTCTracksActive.Inc()

  var transceiver *webrtc.RTPTransceiver

  for _, tr := range p.peerConnection.GetTransceivers() {
    if tr.Receiver() == receiver {
      transceiver = tr

      break
    }
  }

  rti := remoteTrackInfo{trackInfo, transceiver, receiver, track}

  p.addRemoteTrack(rti)
  p.trackEventsCh <- TrackEvent{
    TrackInfo: trackInfo,
    Type:      TrackEventTypeAdd,
  }

  p.wg.Add(1)

  go func() {
    defer func() {
      p.removeRemoteTrack(trackInfo.SSRC)
      p.trackEventsCh <- TrackEvent{
        TrackInfo: trackInfo,
        Type:      TrackEventTypeRemove,
      }

      p.wg.Done()

      prometheusWebRTCTracksActive.Dec()
      prometheusWebRTCTracksDuration.Observe(time.Since(start).Seconds())
    }()

    for {
      pkt, _, err := track.ReadRTP()
      if err != nil {

        if err == io.EOF {
          return
        }

        err = errors.Annotate(err, "read rtp")
        p.log.Printf("[%s] Remote track has ended: %d: %+v", p.clientID, trackInfo.SSRC, err)

        return
      }

      prometheusRTPPacketsReceived.Inc()
      prometheusRTPPacketsReceivedBytes.Add(float64(pkt.MarshalSize()))

      p.rtpLog.Printf("[%s] ReadRTP: %s", p.clientID, pkt)
      p.rtpCh <- pkt
    }
  }()
}

func (p *WebRTCTransport) Signal(payload map[string]interface{}) error {
  err := p.signaller.Signal(payload)

  return errors.Annotate(err, "signal")
}

func (p *WebRTCTransport) SignalChannel() <-chan Payload {
  return p.signaller.SignalChannel()
}

func (p *WebRTCTransport) TrackEventsChannel() <-chan TrackEvent {
  return p.trackEventsCh
}

func (p *WebRTCTransport) RTPChannel() <-chan *rtp.Packet {
  return p.rtpCh
}

func (p *WebRTCTransport) RTCPChannel() <-chan rtcp.Packet {
  return p.rtcpCh
}

func (p *WebRTCTransport) MessagesChannel() <-chan webrtc.DataChannelMessage {
  return p.dataTransceiver.MessagesChannel()
}
