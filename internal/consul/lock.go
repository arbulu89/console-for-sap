package consul

import (
	"fmt"
	"time"

	consulApi "github.com/hashicorp/consul/api"
)

const (
	sessionName      string = "trento-session"
	monitorRetries   int    = 10
	monitorRetryTime        = 100 * time.Millisecond
	waitTime                = 15 * time.Second
)

// Created to enable unit testing
// Disable the next line to create/update the MockGetLock struct
//type GetLock func(c *client, prefix string, q *consulApi.QueryOptions) (*consulApi.KVPair, *consulApi.QueryMeta, error)

var getLock = func(c *client, prefix string, q *consulApi.QueryOptions) (*consulApi.KVPair, *consulApi.QueryMeta, error) {
	return c.KV().Get(prefix, q)
}

var lock = func(l *consulApi.Lock, stopCh <-chan struct{}) (<-chan struct{}, error) {
	return l.Lock(stopCh)
}

func (c *client) AcquireLockKey(prefix string) (*consulApi.Lock, error) {
	opts := &consulApi.LockOptions{
		Key:              prefix,
		SessionName:      sessionName,
		MonitorRetries:   monitorRetries,
		MonitorRetryTime: monitorRetryTime,
	}

	l, err := c.wrapped.LockOpts(opts)
	if err != nil {
		return nil, err
	}

	_, err = lock(l, nil)
	if err != nil {
		return nil, err
	}

	return l, err
}

func (c *client) WaitLock(prefix string) error {
	q := consulApi.QueryOptions{
		WaitTime: waitTime,
	}

	for true {
		// Look for an existing lock, blocking until not taken
		// Based on: https://github.com/hashicorp/consul/blob/master/api/lock.go#L181
		pair, meta, err := getLock(c, prefix, &q)
		if err != nil {
			return fmt.Errorf("failed to read lock: %v", err)
		}

		if pair != nil && pair.Flags != consulApi.LockFlagValue {
			return consulApi.ErrLockConflict
		}

		if pair != nil && pair.Session != "" {
			q.WaitIndex = meta.LastIndex
			continue
		}

		// Exist from the loop as the lock is free
		break
	}

	return nil
}
