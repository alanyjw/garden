package client

import (
	"errors"
	"fmt"

	"github.com/cloudfoundry-incubator/garden/client/connection"
	"github.com/cloudfoundry-incubator/garden/warden"
)

type Client interface {
	warden.Client
}

type ConnectionProvider interface {
	ProvideConnection() (connection.Connection, error)
}

var ErrContainerNotFound = errors.New("container not found")

const MaxIdleConnections = 20

type client struct {
	pool *connectionPool
}

func New(connectionProvider ConnectionProvider) Client {
	return &client{
		pool: &connectionPool{
			connectionProvider: connectionProvider,
			connections:        make(chan connection.Connection, MaxIdleConnections),
		},
	}
}

func (client *client) Create(spec warden.ContainerSpec) (warden.Container, error) {
	conn := client.pool.Acquire()
	defer client.pool.Release(conn)

	res, err := conn.Create(spec)
	if err != nil {
		return nil, err
	}

	return newContainer(res.GetHandle(), client.pool), nil
}

func (client *client) Containers() ([]warden.Container, error) {
	conn := client.pool.Acquire()
	defer client.pool.Release(conn)

	res, err := conn.List(nil)
	if err != nil {
		return nil, err
	}

	containers := []warden.Container{}
	for _, handle := range res.GetHandles() {
		containers = append(containers, newContainer(handle, client.pool))
	}

	return containers, nil
}

func (client *client) Destroy(handle string) error {
	conn := client.pool.Acquire()
	defer client.pool.Release(conn)

	_, err := conn.Destroy(handle)
	if err != nil {
		return err
	}

	return nil
}

func (client *client) Lookup(handle string) (warden.Container, error) {
	conn := client.pool.Acquire()
	defer client.pool.Release(conn)

	res, err := conn.List(nil)
	if err != nil {
		return nil, err
	}

	for _, h := range res.GetHandles() {
		if h == handle {
			return newContainer(handle, client.pool), nil
		}
	}

	return nil, fmt.Errorf("container not found: %s", handle)
}
