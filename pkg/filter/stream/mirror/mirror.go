package mirror

import (
	"context"
	"net"

	"mosn.io/api"
	mosnctx "mosn.io/mosn/pkg/context"
	"mosn.io/mosn/pkg/log"
	"mosn.io/mosn/pkg/protocol"
	"mosn.io/mosn/pkg/protocol/xprotocol"
	"mosn.io/mosn/pkg/types"
	"mosn.io/mosn/pkg/upstream/cluster"
	"mosn.io/pkg/buffer"
	"mosn.io/pkg/utils"
)

type mirror struct {
	amplification  int
	receiveHandler api.StreamReceiverFilterHandler

	dp api.Protocol
	up api.Protocol

	ctx      context.Context
	headers  api.HeaderMap
	data     buffer.IoBuffer
	trailers api.HeaderMap

	clusterName string
	cluster     types.ClusterInfo

	sender types.StreamSender
	host   types.Host
}

func (m *mirror) SetReceiveFilterHandler(handler api.StreamReceiverFilterHandler) {
	m.receiveHandler = handler
}

func (m *mirror) OnReceive(ctx context.Context, headers api.HeaderMap, buf buffer.IoBuffer, trailers api.HeaderMap) api.StreamFilterStatus {
	// router := m.receiveHandler.Route().RouteRule().Policy().HashPolicy()

	// TODO if need mirror

	utils.GoWithRecover(func() {
		clusterManager := cluster.NewClusterManagerSingleton(nil, nil)

		clusterName := "http1_mirror"

		m.ctx = mosnctx.WithValue(mosnctx.Clone(ctx), types.ContextKeyBufferPoolCtx, nil)
		if headers != nil {
			// ! xprotocol should reimplement Clone function, not use default, trans protocol.CommonHeader
			// nolint
			if _, ok := headers.(xprotocol.XFrame); ok {
				h := headers.Clone()
				// nolint
				if _, ok = h.(protocol.CommonHeader); ok {
					log.DefaultLogger.Errorf("not support mirror, protocal {%v} must implement Clone function", mosnctx.Get(m.ctx, types.ContextKeyDownStreamProtocol))
					return
				}
				m.headers = h
			} else {
				// ! http1 and http2 use default Clone function
				m.headers = headers.Clone()
			}
		}
		if buf != nil {
			m.data = buf.Clone()
		}
		if trailers != nil {
			m.trailers = trailers.Clone()
		}

		m.dp, m.up = m.convertProtocol()

		snap := clusterManager.GetClusterSnapshot(ctx, clusterName)
		if snap == nil {
			log.DefaultLogger.Errorf("mirror cluster {%s} not found", clusterName)
			return
		}
		m.cluster = snap.ClusterInfo()
		m.clusterName = clusterName

		for i := 0; i < m.amplification; i++ {
			connPool := clusterManager.ConnPoolForCluster(m, snap, m.up)
			if m.up == protocol.HTTP1 {
				// ! http1 use fake receiver reduce connect
				connPool.NewStream(m.ctx, &receiver{}, m)
			} else {
				connPool.NewStream(m.ctx, nil, m)
			}
		}
	}, nil)
	return api.StreamFilterContinue
}

func (m *mirror) OnDestroy() {}

func (m *mirror) convertProtocol() (dp, up types.ProtocolName) {
	dp = m.getDownStreamProtocol()
	up = m.getUpstreamProtocol()
	return
}

func (m *mirror) getDownStreamProtocol() (prot types.ProtocolName) {
	if dp, ok := mosnctx.Get(m.ctx, types.ContextKeyConfigDownStreamProtocol).(string); ok {
		return types.ProtocolName(dp)
	}
	return m.receiveHandler.RequestInfo().Protocol()
}

func (m *mirror) getUpstreamProtocol() (currentProtocol types.ProtocolName) {
	configProtocol, ok := mosnctx.Get(m.ctx, types.ContextKeyConfigUpStreamProtocol).(string)
	if !ok {
		configProtocol = string(protocol.Xprotocol)
	}

	if m.receiveHandler.Route() != nil && m.receiveHandler.Route().RouteRule() != nil && m.receiveHandler.Route().RouteRule().UpstreamProtocol() != "" {
		configProtocol = m.receiveHandler.Route().RouteRule().UpstreamProtocol()
	}

	if configProtocol == string(protocol.Auto) {
		currentProtocol = m.getDownStreamProtocol()
	} else {
		currentProtocol = types.ProtocolName(configProtocol)
	}
	return currentProtocol
}

func (m *mirror) MetadataMatchCriteria() api.MetadataMatchCriteria {
	return nil
}

func (m *mirror) DownstreamConnection() net.Conn {
	return m.receiveHandler.Connection().RawConn()
}

func (m *mirror) DownstreamHeaders() types.HeaderMap {
	return m.headers
}

func (m *mirror) DownstreamContext() context.Context {
	return m.ctx
}

func (m *mirror) DownstreamCluster() types.ClusterInfo {
	return m.cluster
}

func (m *mirror) DownstreamRoute() api.Route {
	return m.receiveHandler.Route()
}

func (m *mirror) OnFailure(reason types.PoolFailureReason, host types.Host) {}

func (m *mirror) OnReady(sender types.StreamSender, host types.Host) {
	m.sender = sender
	m.host = host

	m.sendDataOnce()
}

func (m *mirror) sendDataOnce() {
	endStream := m.data == nil && m.trailers == nil

	m.sender.AppendHeaders(m.ctx, m.coverHeader(), endStream)

	if endStream {
		return
	}

	endStream = m.trailers == nil
	m.sender.AppendData(m.ctx, m.converData(), endStream)

	if endStream {
		return
	}

	m.sender.AppendTrailers(m.ctx, m.convertTrailer())
}

func (m *mirror) coverHeader() types.HeaderMap {
	if m.dp != m.up {
		convHeader, err := protocol.ConvertHeader(m.ctx, m.dp, m.up, m.headers)
		if err == nil {
			return convHeader
		}
		log.Proxy.Warnf(m.ctx, "[proxy] [upstream] [mirror] convert header from %s to %s failed, %s", m.dp, m.up, err.Error())
	}
	return m.headers
}

func (m *mirror) converData() types.IoBuffer {
	if m.dp != m.up {
		convData, err := protocol.ConvertData(m.ctx, m.dp, m.up, m.data)
		if err == nil {
			return convData
		}
		log.Proxy.Warnf(m.ctx, "[proxy] [upstream] [mirror] convert data from %s to %s failed, %s", m.dp, m.up, err.Error())
	}
	return m.data
}

func (m *mirror) convertTrailer() types.HeaderMap {
	if m.dp != m.up {
		convTrailers, err := protocol.ConvertTrailer(m.ctx, m.dp, m.up, m.trailers)
		if err == nil {
			return convTrailers
		}
		log.Proxy.Warnf(m.ctx, "[proxy] [upstream] [mirror] convert trailers from %s to %s failed, %s", m.dp, m.up, err.Error())
	}
	return m.trailers
}
