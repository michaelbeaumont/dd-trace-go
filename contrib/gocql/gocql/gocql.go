// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

// Package gocql provides functions to trace the gocql/gocql package (https://github.com/gocql/gocql).
package gocql // import "gopkg.in/DataDog/dd-trace-go.v1/contrib/gocql/gocql"

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"unsafe"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/ext"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/telemetry"

	"github.com/gocql/gocql"
)

const componentName = "gocql/gocql"

func init() {
	telemetry.LoadIntegration(componentName)
}

// Query inherits from gocql.Query, it keeps the tracer and the context.
type Query struct {
	*gocql.Query
	*params
	ctx context.Context
}

// Iter inherits from gocql.Iter and contains a span.
type Iter struct {
	*gocql.Iter
	span ddtrace.Span
}

// Scanner inherits from a gocql.Scanner derived from an Iter
type Scanner struct {
	gocql.Scanner
	span ddtrace.Span
}

// Batch inherits from gocql.Batch, it keeps the tracer and the context.
type Batch struct {
	*gocql.Batch
	*params
	ctx context.Context
}

// params containes fields and metadata useful for command tracing
type params struct {
	config               *queryConfig
	keyspace             string
	paginated            bool
	clusterContactPoints string
}

// WrapQuery wraps a gocql.Query into a traced Query under the given service name.
// Note that the returned Query structure embeds the original gocql.Query structure.
// This means that any method returning the query for chaining that is not part
// of this package's Query structure should be called before WrapQuery, otherwise
// the tracing context could be lost.
//
// To be more specific: it is ok (and recommended) to use and chain the return value
// of `WithContext` and `PageState` but not that of `Consistency`, `Trace`,
// `Observer`, etc.
func WrapQuery(q *gocql.Query, opts ...WrapOption) *Query {
	cfg := new(queryConfig)
	defaults(cfg)
	for _, fn := range opts {
		fn(cfg)
	}
	if cfg.resourceName == "" {
		if parts := strings.SplitN(q.String(), "\"", 3); len(parts) == 3 {
			cfg.resourceName = parts[1]
		}
	}
	p := &params{config: cfg}
	cp, ok := getClusterContactPoints(q)
	if ok {
		p.clusterContactPoints = strings.Join(cp, ",")
	}
	log.Debug("contrib/gocql/gocql: Wrapping Query: %#v", cfg)
	tq := &Query{Query: q, params: p, ctx: q.Context()}
	return tq
}

// WithContext adds the specified context to the traced Query structure.
func (tq *Query) WithContext(ctx context.Context) *Query {
	tq.ctx = ctx
	tq.Query = tq.Query.WithContext(ctx)
	return tq
}

// PageState rewrites the original function so that spans are aware of the change.
func (tq *Query) PageState(state []byte) *Query {
	tq.params.paginated = true
	tq.Query = tq.Query.PageState(state)
	return tq
}

// NewChildSpan creates a new span from the params and the context.
func (tq *Query) newChildSpan(ctx context.Context) ddtrace.Span {
	p := tq.params
	opts := []ddtrace.StartSpanOption{
		tracer.SpanType(ext.SpanTypeCassandra),
		tracer.ServiceName(p.config.serviceName),
		tracer.ResourceName(p.config.resourceName),
		tracer.Tag(ext.CassandraPaginated, fmt.Sprintf("%t", p.paginated)),
		tracer.Tag(ext.CassandraKeyspace, p.keyspace),
		tracer.Tag(ext.Component, componentName),
		tracer.Tag(ext.SpanKind, ext.SpanKindClient),
		tracer.Tag(ext.DBSystem, ext.DBSystemCassandra),
	}
	if !math.IsNaN(p.config.analyticsRate) {
		opts = append(opts, tracer.Tag(ext.EventSampleRate, p.config.analyticsRate))
	}
	if tq.clusterContactPoints != "" {
		opts = append(opts, tracer.Tag(ext.CassandraContactPoints, tq.clusterContactPoints))
	}
	span, _ := tracer.StartSpanFromContext(ctx, p.config.querySpanName, opts...)
	return span
}

func (tq *Query) finishSpan(span ddtrace.Span, err error) {
	if err != nil && tq.params.config.shouldIgnoreError(err) {
		err = nil
	}
	if tq.params.config.noDebugStack {
		span.Finish(tracer.WithError(err), tracer.NoDebugStack())
	} else {
		span.Finish(tracer.WithError(err))
	}
}

// Exec is rewritten so that it passes by our custom Iter
func (tq *Query) Exec() error {
	return tq.Iter().Close()
}

// MapScan wraps in a span query.MapScan call.
func (tq *Query) MapScan(m map[string]interface{}) error {
	span := tq.newChildSpan(tq.ctx)
	err := tq.Query.MapScan(m)
	tq.finishSpan(span, err)
	return err
}

// Scan wraps in a span query.Scan call.
func (tq *Query) Scan(dest ...interface{}) error {
	span := tq.newChildSpan(tq.ctx)
	err := tq.Query.Scan(dest...)
	tq.finishSpan(span, err)
	return err
}

// ScanCAS wraps in a span query.ScanCAS call.
func (tq *Query) ScanCAS(dest ...interface{}) (applied bool, err error) {
	span := tq.newChildSpan(tq.ctx)
	applied, err = tq.Query.ScanCAS(dest...)
	tq.finishSpan(span, err)
	return applied, err
}

// Iter starts a new span at query.Iter call.
func (tq *Query) Iter() *Iter {
	span := tq.newChildSpan(tq.ctx)
	iter := tq.Query.Iter()
	span.SetTag(ext.CassandraRowCount, strconv.Itoa(iter.NumRows()))
	span.SetTag(ext.CassandraConsistencyLevel, tq.GetConsistency().String())

	columns := iter.Columns()
	if len(columns) > 0 {
		span.SetTag(ext.CassandraKeyspace, columns[0].Keyspace)
	}
	tIter := &Iter{iter, span}
	if tIter.Host() != nil {
		tIter.span.SetTag(ext.TargetHost, tIter.Iter.Host().HostID())
		tIter.span.SetTag(ext.TargetPort, strconv.Itoa(tIter.Iter.Host().Port()))
		tIter.span.SetTag(ext.CassandraCluster, tIter.Iter.Host().DataCenter())
	}
	return tIter
}

// Close closes the Iter and finish the span created on Iter call.
func (tIter *Iter) Close() error {
	err := tIter.Iter.Close()
	if err != nil {
		tIter.span.SetTag(ext.Error, err)
	}
	tIter.span.Finish()
	return err
}

// Scanner returns a row Scanner which provides an interface to scan rows in a
// manner which is similar to database/sql. The Iter should NOT be used again after
// calling this method.
func (tIter *Iter) Scanner() gocql.Scanner {
	return &Scanner{
		Scanner: tIter.Iter.Scanner(),
		span:    tIter.span,
	}
}

// Err calls the wrapped Scanner.Err, releasing the Scanner resources and closing the span.
func (s *Scanner) Err() error {
	err := s.Scanner.Err()
	if err != nil {
		s.span.SetTag(ext.Error, err)
	}
	s.span.Finish()
	return err
}

// WrapBatch wraps a gocql.Batch into a traced Batch under the given service name.
// Note that the returned Batch structure embeds the original gocql.Batch structure.
// This means that any method returning the batch for chaining that is not part
// of this package's Batch structure should be called before WrapBatch, otherwise
// the tracing context could be lost.
//
// To be more specific: it is ok (and recommended) to use and chain the return value
// of `WithContext` and `WithTimestamp` but not that of `SerialConsistency`, `Trace`,
// `Observer`, etc.
func WrapBatch(b *gocql.Batch, opts ...WrapOption) *Batch {
	cfg := new(queryConfig)
	defaults(cfg)
	for _, fn := range opts {
		fn(cfg)
	}
	p := &params{config: cfg}
	cp, ok := getClusterContactPoints(b)
	if ok {
		p.clusterContactPoints = strings.Join(cp, ",")
	}
	log.Debug("contrib/gocql/gocql: Wrapping Batch: %#v", cfg)
	tb := &Batch{Batch: b, params: p, ctx: b.Context()}
	return tb
}

// WithContext adds the specified context to the traced Batch structure.
func (tb *Batch) WithContext(ctx context.Context) *Batch {
	tb.ctx = ctx
	tb.Batch = tb.Batch.WithContext(ctx)
	return tb
}

// WithTimestamp will enable the with default timestamp flag on the query like
// DefaultTimestamp does. But also allows to define value for timestamp. It works the
// same way as USING TIMESTAMP in the query itself, but should not break prepared
// query optimization.
func (tb *Batch) WithTimestamp(timestamp int64) *Batch {
	tb.Batch = tb.Batch.WithTimestamp(timestamp)
	return tb
}

// ExecuteBatch calls session.ExecuteBatch on the Batch, tracing the execution.
func (tb *Batch) ExecuteBatch(session *gocql.Session) error {
	span := tb.newChildSpan(tb.ctx)
	err := session.ExecuteBatch(tb.Batch)
	tb.finishSpan(span, err)
	return err
}

// newChildSpan creates a new span from the params and the context.
func (tb *Batch) newChildSpan(ctx context.Context) ddtrace.Span {
	p := tb.params
	opts := []ddtrace.StartSpanOption{
		tracer.SpanType(ext.SpanTypeCassandra),
		tracer.ServiceName(p.config.serviceName),
		tracer.ResourceName(p.config.resourceName),
		tracer.Tag(ext.CassandraConsistencyLevel, tb.Cons.String()),
		tracer.Tag(ext.CassandraKeyspace, tb.Keyspace()),
		tracer.Tag(ext.Component, componentName),
		tracer.Tag(ext.SpanKind, ext.SpanKindClient),
		tracer.Tag(ext.DBSystem, ext.DBSystemCassandra),
	}
	if !math.IsNaN(p.config.analyticsRate) {
		opts = append(opts, tracer.Tag(ext.EventSampleRate, p.config.analyticsRate))
	}
	if tb.clusterContactPoints != "" {
		opts = append(opts, tracer.Tag(ext.CassandraContactPoints, tb.clusterContactPoints))
	}
	span, _ := tracer.StartSpanFromContext(ctx, p.config.batchSpanName, opts...)
	return span
}

func (tb *Batch) finishSpan(span ddtrace.Span, err error) {
	if err != nil && tb.params.config.shouldIgnoreError(err) {
		err = nil
	}
	if tb.params.config.noDebugStack {
		span.Finish(tracer.WithError(err), tracer.NoDebugStack())
	} else {
		span.Finish(tracer.WithError(err))
	}
}

type query interface {
	*gocql.Query | *gocql.Batch
}

func getClusterContactPoints[T query](q T) ([]string, bool) {
	defer func() { recover() }()
	sv := reflect.ValueOf(q).Elem().FieldByName("session")
	sv = reflect.NewAt(sv.Type(), unsafe.Pointer(sv.UnsafeAddr())).Elem()
	s, ok := sv.Interface().(*gocql.Session)
	if !ok {
		return nil, false
	}
	cfg, ok := getClusterConfig(s)
	if !ok {
		return nil, false
	}
	return cfg.Hosts, true
}

func getClusterConfig(s *gocql.Session) (gocql.ClusterConfig, bool) {
	defer func() { recover() }()
	cv := reflect.ValueOf(s).Elem().FieldByName("cfg")
	cv = reflect.NewAt(cv.Type(), unsafe.Pointer(cv.UnsafeAddr())).Elem()
	cfg, ok := cv.Interface().(gocql.ClusterConfig)
	return cfg, ok
}
