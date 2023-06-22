// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

package grpc

import (
	"encoding/json"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo/instrumentation"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo/instrumentation/grpcsec"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo/instrumentation/httpsec"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo/instrumentation/sharedsec"

	"github.com/DataDog/appsec-internal-go/netip"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// UnaryHandler wrapper to use when AppSec is enabled to monitor its execution.
func appsecUnaryHandlerMiddleware(span ddtrace.Span, handler grpc.UnaryHandler) grpc.UnaryHandler {
	instrumentation.SetAppSecEnabledTags(span)
	return func(ctx context.Context, req interface{}) (interface{}, error) {
		var err error
		md, _ := metadata.FromIncomingContext(ctx)
		clientIP := setClientIP(ctx, span, md)
		op := grpcsec.NewHandlerOperation(nil)
		sharedsec.OnData(op, func(a *sharedsec.Action) {
			code, e := a.GRPC()(md)
			op.AddTag(instrumentation.BlockedRequestTag, true)
			err = status.Error(codes.Code(code), e.Error())
		})
		ctx = grpcsec.StartHandlerOperation(ctx, op, grpcsec.HandlerOperationArgs{Metadata: md, ClientIP: clientIP})
		defer func() {
			events := op.Finish(grpcsec.HandlerOperationRes{})
			instrumentation.SetTags(span, op.Tags())
			if len(events) == 0 {
				return
			}
			setAppSecEventsTags(ctx, span, events)
		}()

		if err != nil {
			return nil, err
		}
		defer grpcsec.StartReceiveOperation(grpcsec.ReceiveOperationArgs{}, op).Finish(grpcsec.ReceiveOperationRes{Message: req})
		return handler(ctx, req)
	}
}

// StreamHandler wrapper to use when AppSec is enabled to monitor its execution.
func appsecStreamHandlerMiddleware(span ddtrace.Span, handler grpc.StreamHandler) grpc.StreamHandler {
	instrumentation.SetAppSecEnabledTags(span)
	return func(srv interface{}, stream grpc.ServerStream) error {
		var err error
		ctx := stream.Context()
		md, _ := metadata.FromIncomingContext(ctx)
		clientIP := setClientIP(ctx, span, md)

		op := grpcsec.NewHandlerOperation(nil)
		sharedsec.OnData(op, func(a *sharedsec.Action) {
			code, e := a.GRPC()(md)
			op.AddTag(instrumentation.BlockedRequestTag, true)
			err = status.Error(codes.Code(code), e.Error())
		})
		ctx = grpcsec.StartHandlerOperation(ctx, op, grpcsec.HandlerOperationArgs{Metadata: md, ClientIP: clientIP})
		dyngo.StartOperation(op, grpcsec.HandlerOperationArgs{Metadata: md, ClientIP: clientIP})
		stream = appsecServerStream{
			ServerStream:     stream,
			handlerOperation: op,
			ctx:              ctx,
		}
		defer func() {
			events := op.Finish(grpcsec.HandlerOperationRes{})
			instrumentation.SetTags(span, op.Tags())
			if len(events) == 0 {
				return
			}
			setAppSecEventsTags(stream.Context(), span, events)
		}()

		if err != nil {
			return err
		}

		return handler(srv, stream)
	}
}

type appsecServerStream struct {
	grpc.ServerStream
	handlerOperation *grpcsec.HandlerOperation
	ctx              context.Context
}

// RecvMsg implements grpc.ServerStream interface method to monitor its
// execution with AppSec.
func (ss appsecServerStream) RecvMsg(m interface{}) error {
	op := grpcsec.StartReceiveOperation(grpcsec.ReceiveOperationArgs{}, ss.handlerOperation)
	defer func() {
		op.Finish(grpcsec.ReceiveOperationRes{Message: m})
	}()
	return ss.ServerStream.RecvMsg(m)
}

func (ss appsecServerStream) Context() context.Context {
	return ss.ctx
}

// Set the AppSec tags when security events were found.
func setAppSecEventsTags(ctx context.Context, span ddtrace.Span, events []json.RawMessage) {
	md, _ := metadata.FromIncomingContext(ctx)
	grpcsec.SetSecurityEventTags(span, events, md)
}

func setClientIP(ctx context.Context, span ddtrace.Span, md metadata.MD) netip.Addr {
	var remoteAddr string
	if p, ok := peer.FromContext(ctx); ok {
		remoteAddr = p.Addr.String()
	}
	ipTags, clientIP := httpsec.ClientIPTags(md, false, remoteAddr)
	if len(ipTags) > 0 {
		instrumentation.SetStringTags(span, ipTags)
	}
	return clientIP
}
