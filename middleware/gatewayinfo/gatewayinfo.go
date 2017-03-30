// Copyright © 2017 The Things Network
// Use of this source code is governed by the MIT license that can be found in the LICENSE file.

package gatewayinfo

import (
	"strings"
	"sync"
	"time"

	"github.com/TheThingsNetwork/gateway-connector-bridge/middleware"
	"github.com/TheThingsNetwork/gateway-connector-bridge/types"
	"github.com/TheThingsNetwork/go-account-lib/account"
	"github.com/TheThingsNetwork/go-utils/log"
	"github.com/TheThingsNetwork/ttn/api/gateway"
)

// RequestInterval sets how often the account server may be queried
var RequestInterval = 50 * time.Millisecond

// RequestBurst sets the burst of requests to the account server
var RequestBurst = 50

// NewPublic returns a middleware that injects public gateway information
func NewPublic(accountServer string) *Public {
	p := &Public{
		log:       log.Get(),
		account:   account.New(accountServer),
		info:      make(map[string]*info),
		available: make(chan struct{}, RequestBurst),
	}
	for i := 0; i < RequestBurst; i++ {
		p.available <- struct{}{}
	}
	go func() {
		for range time.Tick(RequestInterval) {
			select {
			case p.available <- struct{}{}:
			default:
			}
		}
	}()
	return p
}

// WithExpire adds an expiration to gateway information. Information is re-fetched if expired
func (p *Public) WithExpire(duration time.Duration) *Public {
	p.expire = duration
	return p
}

// Public gateway information will be injected
type Public struct {
	log     log.Interface
	account *account.Account
	expire  time.Duration

	mu   sync.Mutex
	info map[string]*info

	available chan struct{}
}

type info struct {
	lastUpdated time.Time
	err         error
	gateway     account.Gateway
}

func (p *Public) fetch(gatewayID string) error {
	<-p.available
	gateway, err := p.account.FindGateway(gatewayID)
	if err != nil {
		p.setErr(gatewayID, err)
		return err
	}
	p.set(gatewayID, gateway)
	return nil
}

func (p *Public) setErr(gatewayID string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gtw, ok := p.info[gatewayID]; ok {
		gtw.lastUpdated = time.Now()
		gtw.err = err
	} else {
		p.info[gatewayID] = &info{
			lastUpdated: time.Now(),
			err:         err,
		}
	}
}

func (p *Public) set(gatewayID string, gateway account.Gateway) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.info[gatewayID] = &info{
		lastUpdated: time.Now(),
		gateway:     gateway,
	}
}

func (p *Public) get(gatewayID string) (gateway account.Gateway, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	info, ok := p.info[gatewayID]
	if !ok {
		return gateway, nil
	}
	if p.expire != 0 && time.Since(info.lastUpdated) > p.expire {
		info.lastUpdated = time.Now()
		go p.fetch(gatewayID)
	}
	return info.gateway, info.err
}

func (p *Public) unset(gatewayID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.info, gatewayID)
}

// HandleConnect fetches public gateway information in the background when a ConnectMessage is received
func (p *Public) HandleConnect(ctx middleware.Context, msg *types.ConnectMessage) error {
	go func() {
		log := p.log.WithField("GatewayID", msg.GatewayID)
		err := p.fetch(msg.GatewayID)
		if err != nil {
			log.WithError(err).Warn("Could not get public Gateway information")
		} else {
			log.Debug("Got public Gateway information")
		}
	}()
	return nil
}

// HandleDisconnect cleans up
func (p *Public) HandleDisconnect(ctx middleware.Context, msg *types.DisconnectMessage) error {
	go p.unset(msg.GatewayID)
	return nil
}

// HandleUplink inserts metadata if set in info, but not present in message
func (p *Public) HandleUplink(ctx middleware.Context, msg *types.UplinkMessage) error {
	info, err := p.get(msg.GatewayID)
	if err != nil {
		msg.Message.Trace = msg.Message.Trace.WithEvent("unable to get gateway info", "error", err)
	}
	meta := msg.Message.GetGatewayMetadata()
	if meta.Gps == nil && info.AntennaLocation != nil {
		msg.Message.Trace = msg.Message.Trace.WithEvent("injecting gateway location")
		meta.Gps = &gateway.GPSMetadata{
			Latitude:  float32(info.AntennaLocation.Latitude),
			Longitude: float32(info.AntennaLocation.Longitude),
			Altitude:  int32(info.AntennaLocation.Altitude),
		}
	}
	return nil
}

// HandleStatus inserts metadata if set in info, but not present in message
func (p *Public) HandleStatus(ctx middleware.Context, msg *types.StatusMessage) error {
	info, _ := p.get(msg.GatewayID)
	if msg.Message.Gps == nil && info.AntennaLocation != nil {
		msg.Message.Gps = &gateway.GPSMetadata{
			Latitude:  float32(info.AntennaLocation.Latitude),
			Longitude: float32(info.AntennaLocation.Longitude),
			Altitude:  int32(info.AntennaLocation.Altitude),
		}
	}
	if msg.Message.FrequencyPlan == "" && info.FrequencyPlan != "" {
		msg.Message.FrequencyPlan = info.FrequencyPlan
	}
	if msg.Message.Platform == "" {
		platform := []string{}
		if info.Attributes.Brand != nil {
			platform = append(platform, *info.Attributes.Brand)
		}
		if info.Attributes.Model != nil {
			platform = append(platform, *info.Attributes.Model)
		}
		msg.Message.Platform = strings.Join(platform, " ")
	}
	if msg.Message.Description == "" && info.Attributes.Description != nil {
		msg.Message.Description = *info.Attributes.Description
	}
	return nil
}
