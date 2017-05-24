/*-
 * Copyright (c) 2017, F5 Networks, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package f5router

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/cf-bigip-ctlr/config"
	"github.com/cf-bigip-ctlr/logger"
	"github.com/cf-bigip-ctlr/route"

	"github.com/uber-go/zap"
	"k8s.io/client-go/util/workqueue"
)

const (
	add operation = iota
	remove
)

const (
	// HTTP virtual server without SSL termination on port 80
	HTTP vsType = iota
	// HTTPS virtual server with SSL termination on port 443
	HTTPS
)

const (
	// HTTPRouterName HTTP virtual server name
	HTTPRouterName = "routing-vip-http"
	// HTTPSRouterName HTTPS virtual server name
	HTTPSRouterName = "routing-vip-https"
	// CFRoutingPolicyName Policy name for CF routing
	CFRoutingPolicyName = "cf-routing-policy"
)

func (r rules) Len() int           { return len(r) }
func (r rules) Less(i, j int) bool { return r[i].FullURI < r[j].FullURI }
func (r rules) Swap(i, j int)      { r[i], r[j] = r[j], r[i] }

func (t vsType) String() string {
	switch t {
	case HTTP:
		return "HTTP"
	case HTTPS:
		return "HTTPS"
	}

	return "Unknown"
}

// NewF5Router create the F5Router route controller
func NewF5Router(logger logger.Logger, c *config.Config) (*F5Router, error) {
	writer, err := NewConfigWriter(logger)
	if nil != err {
		return nil, err
	}
	r := F5Router{
		c:         c,
		logger:    logger,
		m:         make(routeMap),
		r:         make(ruleMap),
		wildcards: make(ruleMap),
		queue:     workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		writer:    writer,
	}

	err = r.writeInitialConfig()
	if nil != err {
		return nil, err
	}
	return &r, nil
}

// ConfigWriter return the internal config writer instance
func (r *F5Router) ConfigWriter() *ConfigWriter {
	return r.writer
}

// Run start the F5Router controller
func (r *F5Router) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	r.logger.Info("f5router-starting")

	done := make(chan struct{})
	go r.runWorker(done)

	close(ready)
	<-signals
	r.queue.ShutDown()
	<-done
	r.logger.Info("f5router-exited")
	return nil
}

func (r *F5Router) writeInitialConfig() error {
	sections := make(map[string]interface{})
	sections["global"] = config.GlobalSection{
		LogLevel:       r.c.Logging.Level,
		VerifyInterval: r.c.BigIP.VerifyInterval,
	}
	sections["bigip"] = r.c.BigIP

	output, err := json.Marshal(sections)
	if nil != err {
		return fmt.Errorf("failed marshaling initial config: %v", err)
	}
	n, err := r.writer.Write(output)
	if nil != err {
		return fmt.Errorf("failed writing initial config: %v", err)
	} else if len(output) != n {
		return fmt.Errorf("short write from initial config")
	}

	return nil
}

func (r *F5Router) runWorker(done chan<- struct{}) {
	r.logger.Debug("f5router-starting-worker")
	for r.process() {
	}
	r.logger.Debug("f5router-stopping-worker")
	close(done)
}

func (r *F5Router) generateNameList(names []string) []*nameRef {
	var refs []*nameRef
	for i := range names {
		p := strings.TrimPrefix(names[i], "/")
		parts := strings.Split(p, "/")
		if 2 == len(parts) {
			refs = append(refs, &nameRef{
				Name:      parts[1],
				Partition: parts[0],
			})
		} else {
			r.logger.Warn("f5router-skipping-name",
				zap.String("parse-error",
					fmt.Sprintf("skipping name %s, need format /[partition]/[name]",
						p)),
			)
		}
	}

	return refs
}

func (r *F5Router) generatePolicyList() []*nameRef {
	var n []*nameRef
	n = append(n, r.generateNameList(r.c.BigIP.Policies.PreRouting)...)
	n = append(n, &nameRef{
		Name:      CFRoutingPolicyName,
		Partition: r.c.BigIP.Partitions[0], // FIXME handle multiple partitions
	})
	n = append(n, r.generateNameList(r.c.BigIP.Policies.PostRouting)...)
	return n
}

func (r *F5Router) process() bool {
	item, quit := r.queue.Get()
	if quit {
		r.logger.Debug("f5router-quit-signal-received")
		return false
	}

	defer r.queue.Done(item)

	workItem, ok := item.(workItem)
	if false == ok {
		r.logger.Warn("f5router-unknown-workitem",
			zap.Error(errors.New("workqueue delivered unsupported work type")))
		return true
	}

	var tryUpdate bool
	var err error
	switch work := workItem.data.(type) {
	case poolData:
		r.logger.Debug("f5router-received-pool-request")
		tryUpdate, err = r.processPool(workItem.op, work)
	case virtualData:
		r.logger.Debug("f5router-received-virtual-request")
		tryUpdate, err = r.processVirtual(workItem.op, work)
	default:
		r.logger.Warn("f5router-unknown-request",
			zap.Error(errors.New("workqueue item contains unsupported work request")))
	}

	if false == r.drainUpdate {
		r.drainUpdate = tryUpdate
	}

	if nil != err {
		r.logger.Warn("f5router-process-error", zap.Error(err))
	} else {
		l := r.queue.Len()
		if true == r.drainUpdate && 0 == l {
			r.drainUpdate = false

			sections := make(map[string]interface{})
			sections["global"] = config.GlobalSection{
				LogLevel:       r.c.Logging.Level,
				VerifyInterval: r.c.BigIP.VerifyInterval,
			}
			sections["bigip"] = r.c.BigIP

			sections["policies"] = policies{r.makeRoutePolicy(CFRoutingPolicyName)}

			plcs := r.generatePolicyList()
			prfls := r.generateNameList(r.c.BigIP.Profiles)
			if vs, ok := r.m[HTTPRouterName]; ok {
				vs.Item.Frontend.Policies = plcs
				vs.Item.Frontend.Profiles = prfls
			}
			if vs, ok := r.m[HTTPSRouterName]; ok {
				vs.Item.Frontend.Policies = plcs
				vs.Item.Frontend.Profiles = prfls
			}

			services := routeConfigs{}
			for _, rc := range r.m {
				services = append(services, rc)
			}
			sections["services"] = services

			r.logger.Debug("f5router-drain", zap.Object("writing", sections))

			output, err := json.Marshal(sections)
			if nil != err {
				r.logger.Warn("f5router-config-marshal-error", zap.Error(err))
			} else {
				n, err := r.writer.Write(output)
				if nil != err {
					r.logger.Warn("f5router-config-write-error", zap.Error(err))
				} else if len(output) != n {
					r.logger.Warn("f5router-config-short-write", zap.Error(err))
				} else {
					r.logger.Debug("f5router-wrote-config",
						zap.Int("number-services", len(services)),
					)
				}
			}
		} else {
			r.logger.Debug("f5router-write-not-ready",
				zap.Bool("update", r.drainUpdate),
				zap.Int("length", l),
			)
		}
	}
	return true
}

// makePool create Pool-Only configuration item
func (r *F5Router) makePool(
	name string,
	uri string,
	addrs ...string,
) *routeConfig {
	return &routeConfig{
		Item: routeItem{
			Backend: backend{
				ServiceName:     uri,
				ServicePort:     -1, // unused
				PoolMemberAddrs: addrs,
			},
			Frontend: frontend{
				Name: name,
				//FIXME need to handle multiple partitions
				Partition: r.c.BigIP.Partitions[0],
				Balance:   r.c.BigIP.Balance,
				Mode:      "http",
			},
		},
	}
}

func (r *F5Router) makeRouteRule(p poolData) (*rule, error) {
	_u := "scheme://" + p.URI
	_u = strings.TrimSuffix(_u, "/")
	u, err := url.Parse(_u)
	if nil != err {
		return nil, err
	}

	var b bytes.Buffer
	b.WriteRune('/')
	b.WriteString(r.c.BigIP.Partitions[0]) //FIXME update to use mutliple partitions
	b.WriteRune('/')
	b.WriteString(p.Name)

	a := action{
		Forward: true,
		Name:    "0",
		Pool:    b.String(),
		Request: true,
	}

	var c []*condition
	if true == p.Wildcard {
		c = append(c, &condition{
			EndsWith: true,
			Host:     true,
			HTTPHost: true,
			Name:     "0",
			Index:    0,
			Request:  true,
			Values:   []string{u.Host},
		})
	} else {
		c = append(c, &condition{
			Equals:   true,
			Host:     true,
			HTTPHost: true,
			Name:     "0",
			Index:    0,
			Request:  true,
			Values:   []string{u.Host},
		})

		if 0 != len(u.Path) {
			path := strings.TrimPrefix(u.Path, "/")
			segments := strings.Split(path, "/")

			for i, v := range segments {
				c = append(c, &condition{
					Equals:      true,
					HTTPURI:     true,
					PathSegment: true,
					Name:        strconv.Itoa(i + 1),
					Index:       i + 1,
					Request:     true,
					Values:      []string{v},
				})
			}
		}
	}

	rl := rule{
		FullURI:    p.URI,
		Actions:    []*action{&a},
		Conditions: c,
		Name:       p.Name,
	}

	r.logger.Debug("f5router-rule-create", zap.Object("rule", rl))
	return &rl, nil
}

func (r *F5Router) makeRoutePolicy(policyName string) *policy {
	plcy := policy{
		Controls:  []string{"forwarding"},
		Legacy:    true,
		Name:      policyName,
		Partition: r.c.BigIP.Partitions[0], //FIXME handle multiple partitions
		Requires:  []string{"http"},
		Rules:     []*rule{},
		Strategy:  "/Common/first-match",
	}

	var wg sync.WaitGroup
	wg.Add(2)
	sortRules := func(r ruleMap, rls *rules, ordinal int) {
		for _, v := range r {
			*rls = append(*rls, v)
		}

		sort.Sort(sort.Reverse(*rls))

		for _, v := range *rls {
			v.Ordinal = ordinal
			ordinal++
		}
		wg.Done()
	}

	rls := rules{}
	go sortRules(r.r, &rls, 0)

	w := rules{}
	go sortRules(r.wildcards, &w, len(r.m))

	wg.Wait()

	rls = append(rls, w...)

	plcy.Rules = rls

	r.logger.Debug("f5router-policy-create", zap.Object("policy", plcy))
	return &plcy
}

func (r *F5Router) processPool(op operation, p poolData) (bool, error) {
	if op == add {
		return r.processPoolAdd(p), nil
	} else if op == remove {
		return r.processPoolRemove(p), nil
	} else {
		return false, fmt.Errorf("received unsupported pool operation %v", op)
	}
}

func (r *F5Router) processPoolAdd(p poolData) bool {
	var ret bool
	if pool, ok := r.m[p.Name]; ok {
		var found bool
		for _, e := range pool.Item.Backend.PoolMemberAddrs {
			if e == p.Endpoint {
				found = true
				break
			}
		}
		if false == found {
			pool.Item.Backend.PoolMemberAddrs =
				append(pool.Item.Backend.PoolMemberAddrs, p.Endpoint)
			ret = true
			r.logger.Debug("f5router-pool-updated", zap.Object("pool-config", pool))
		} else {
			r.logger.Debug("f5router-pool-not-updated", []zap.Field{
				zap.String("wanted", p.Endpoint),
				zap.Object("have", pool),
			}...)
		}
	} else {
		rule, err := r.makeRouteRule(p)
		if nil != err {
			r.logger.Warn("f5router-rule-error", zap.Error(err))
			return false
		}

		if true == p.Wildcard {
			r.wildcards[p.URI] = rule
			r.logger.Debug("f5router-wildcard-rule-updated",
				zap.String("name", p.Name),
				zap.String("uri", p.URI),
			)
		} else {
			r.r[p.URI] = rule
			r.logger.Debug("f5router-app-rule-updated",
				zap.String("name", p.Name),
				zap.String("uri", p.URI),
			)
		}

		pool := r.makePool(p.Name, p.URI, p.Endpoint)
		r.m[p.Name] = pool
		ret = true
		r.logger.Debug("f5router-pool-created", zap.Object("pool-config", pool))
	}

	return ret
}

func (r *F5Router) processPoolRemove(p poolData) bool {
	var ret bool
	if pool, ok := r.m[p.Name]; ok {
		for i, e := range pool.Item.Backend.PoolMemberAddrs {
			if e == p.Endpoint {
				pool.Item.Backend.PoolMemberAddrs = append(
					pool.Item.Backend.PoolMemberAddrs[:i],
					pool.Item.Backend.PoolMemberAddrs[i+1:]...)
				break
			}
		}
		r.logger.Debug("f5router-pool-endpoint-removed",
			zap.String("removed", p.Endpoint),
			zap.Object("remaining", pool),
		)
		ret = true

		if 0 == len(pool.Item.Backend.PoolMemberAddrs) {
			delete(r.m, p.Name)
			r.logger.Debug("f5router-pool-removed")

			if true == p.Wildcard {
				delete(r.wildcards, p.URI)
				r.logger.Debug("f5router-wildcard-rule-removed",
					zap.String("name", p.Name),
					zap.String("uri", p.URI),
				)
			} else {
				delete(r.r, p.URI)
				r.logger.Debug("f5router-app-rule-removed",
					zap.String("name", p.Name),
					zap.String("uri", p.URI),
				)
			}
		}
	} else {
		r.logger.Debug("f5router-pool-not-found", zap.String("uri", p.Name))
	}

	return ret
}

func makePoolName(uri string) string {
	sum := sha256.Sum256([]byte(uri))
	index := strings.Index(uri, ".")

	name := fmt.Sprintf("%s-%x", uri[:index], sum[:8])
	return name
}

// UpdatePoolEndpoints create Pool-Only config or update existing endpoint
func (r *F5Router) UpdatePoolEndpoints(
	uri string,
	endpoint *route.Endpoint,
) {
	r.logger.Debug("f5router-updating-pool",
		zap.String("uri", uri),
		zap.String("endpoint", endpoint.CanonicalAddr()),
	)

	p := poolData{}
	if strings.HasPrefix(uri, "*.") {
		p.URI = strings.TrimPrefix(uri, "*.")
		p.Name = p.URI
		p.Wildcard = true
	} else {
		p.URI = uri
		p.Name = makePoolName(uri)
	}

	p.Endpoint = endpoint.CanonicalAddr()
	w := workItem{
		op:   add,
		data: p,
	}
	r.queue.Add(w)
}

// RemovePoolEndpoints remove endpoint from config, if empty remove
// VirtualServer
func (r *F5Router) RemovePoolEndpoints(
	uri string,
	endpoint *route.Endpoint,
) {
	r.logger.Debug("f5router-removing-pool",
		zap.String("uri", uri),
		zap.String("endpoint", endpoint.CanonicalAddr()),
	)
	var (
		u    string
		name string
		wild bool
	)
	if strings.HasPrefix(uri, "*.") {
		u = strings.TrimPrefix(uri, "*.")
		name = u
		wild = true
	} else {
		u = uri
		name = makePoolName(uri)
	}

	p := poolData{
		Name:     name,
		URI:      uri,
		Endpoint: endpoint.CanonicalAddr(),
		Wildcard: wild,
	}
	w := workItem{
		op:   remove,
		data: p,
	}
	r.queue.Add(w)
}

func (r *F5Router) makeVirtual(
	name string,
	t vsType,
) *routeConfig {
	var port int32
	var ssl *sslProfile

	if t == HTTP {
		port = 80
	} else if t == HTTPS {
		port = 443
		ssl = &sslProfile{
			F5ProfileName: r.c.BigIP.SSLProfile,
		}
	}

	vs := &routeConfig{
		Item: routeItem{
			Backend: backend{
				ServiceName:     name,
				ServicePort:     -1,         // unused
				PoolMemberAddrs: []string{}, // unused
			},
			Frontend: frontend{
				Name: name,
				//FIXME need to handle multiple partitions
				Partition: r.c.BigIP.Partitions[0],
				Balance:   r.c.BigIP.Balance,
				Mode:      "http",
				VirtualAddress: &virtualAddress{
					BindAddr: r.c.BigIP.ExternalAddr,
					Port:     port,
				},
				SSLProfile: ssl,
			},
		},
	}
	return vs
}

func (r *F5Router) processVirtual(op operation, v virtualData) (bool, error) {
	if op == add {
		return r.processVirtualAdd(v), nil
	} else if op == remove {
		return r.processVirtualRemove(v), nil
	} else {
		return false, fmt.Errorf("received unsupported virtual operation %v", op)
	}
}

func (r *F5Router) processVirtualAdd(v virtualData) bool {
	vs := r.makeVirtual(v.Name, v.T)
	r.m[v.Name] = vs
	r.logger.Debug("f5router-virtual-server-updated", zap.Object("virtual", vs))
	return true
}

func (r *F5Router) processVirtualRemove(v virtualData) bool {
	delete(r.m, v.Name)
	r.logger.Debug("f5router-virtual-server-removed", zap.String("virtual", v.Name))
	return true
}

// UpdateVirtualServer create VirtualServer config with VirtualAddress frontend
func (r *F5Router) UpdateVirtualServer(
	name string,
	t vsType,
) {
	r.logger.Debug("f5router-updating-virtual-server",
		zap.String("name", name),
		zap.String("type", t.String()),
	)
	vs := virtualData{
		Name: name,
		T:    t,
	}

	w := workItem{
		op:   add,
		data: vs,
	}
	r.queue.Add(w)
}

// RemoveVirtualServer delete VirtualServer config
func (r *F5Router) RemoveVirtualServer(
	name string,
	t vsType,
) {
	r.logger.Debug("f5router-removing-virtual-server",
		zap.String("name", name),
		zap.String("type", t.String()),
	)
	vs := virtualData{
		Name: name,
		T:    t,
	}

	w := workItem{
		op:   remove,
		data: vs,
	}
	r.queue.Add(w)
}
