package redis

import (
	"crypto/tls"
	"net"

	stats "github.com/lyft/gostats"
	"github.com/lyft/ratelimit/src/assert"
	"github.com/mediocregopher/radix.v2/pool"
	"github.com/mediocregopher/radix.v2/redis"
	logger "github.com/sirupsen/logrus"
)

type poolStats struct {
	connectionActive stats.Gauge
	connectionTotal  stats.Counter
	connectionClose  stats.Counter
}

func newPoolStats(scope stats.Scope) poolStats {
	ret := poolStats{}
	ret.connectionActive = scope.NewGauge("cx_active")
	ret.connectionTotal = scope.NewCounter("cx_total")
	ret.connectionClose = scope.NewCounter("cx_local_close")
	return ret
}

type poolImpl struct {
	pool  *pool.Pool
	stats poolStats
}

type connectionImpl struct {
	client  *redis.Client
	pending uint
}

type responseImpl struct {
	response *redis.Resp
}

func checkError(err error) {
	if err != nil {
		panic(RedisError(err.Error()))
	}
}

func (this *poolImpl) Get() Connection {
	client, err := this.pool.Get()
	checkError(err)
	this.stats.connectionActive.Inc()
	this.stats.connectionTotal.Inc()
	return &connectionImpl{client, 0}
}

func (this *poolImpl) Put(c Connection) {
	impl := c.(*connectionImpl)
	this.stats.connectionActive.Dec()
	if impl.pending == 0 {
		this.pool.Put(impl.client)
	} else {
		// radix does not appear to track if we attempt to put a connection back with pipelined
		// responses that have not been flushed. If we are in this state, just kill the connection
		// and don't put it back in the pool.
		impl.client.Close()
		this.stats.connectionClose.Inc()
	}
}

func NewPoolImpl(scope stats.Scope, useTls bool, auth string, url string, poolSize int) Pool {
	logger.Warnf("connecting to redis on %s with pool size %d", url, poolSize)
	df := func(network, addr string) (*redis.Client, error) {
		var conn net.Conn
		var err error
		if useTls {
			conn, err = tls.Dial("tcp", addr, &tls.Config{})
		} else {
			conn, err = net.Dial("tcp", addr)
		}
		if err != nil {
			return nil, err
		}
		client, err := redis.NewClient(conn)

		if err != nil {
			return nil, err
		}
		if auth != "" {
			logger.Warnf("enabling authentication to redis on %s", url)
			if err = client.Cmd("AUTH", auth).Err; err != nil {
				client.Close()
				return nil, err
			}
		}
		return client, nil
	}
	pool, err := pool.NewCustom("tcp", url, poolSize, df)
	checkError(err)
	return &poolImpl{
		pool:  pool,
		stats: newPoolStats(scope)}
}

func (this *connectionImpl) PipeAppend(cmd string, args ...interface{}) {
	this.client.PipeAppend(cmd, args...)
	this.pending++
}

func (this *connectionImpl) PipeResponse() Response {
	assert.Assert(this.pending > 0)
	this.pending--

	resp := this.client.PipeResp()
	checkError(resp.Err)
	return &responseImpl{resp}
}

func (this *responseImpl) Int() int64 {
	i, err := this.response.Int64()
	checkError(err)
	return i
}
