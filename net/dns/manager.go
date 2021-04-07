// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dns

import (
	"strings"
	"time"

	"inet.af/netaddr"
	"tailscale.com/net/dns/resolver"
	"tailscale.com/net/tsaddr"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/monitor"
)

// We use file-ignore below instead of ignore because on some platforms,
// the lint exception is necessary and on others it is not,
// and plain ignore complains if the exception is unnecessary.

//lint:file-ignore U1000 reconfigTimeout is used on some platforms but not others

// reconfigTimeout is the time interval within which Manager.{Up,Down} should complete.
//
// This is particularly useful because certain conditions can cause indefinite hangs
// (such as improper dbus auth followed by contextless dbus.Object.Call).
// Such operations should be wrapped in a timeout context.
const reconfigTimeout = time.Second

// Manager manages system DNS settings.
type Manager struct {
	logf logger.Logf

	resolver *resolver.Resolver
	os       OSConfigurator

	config Config
}

// NewManagers created a new manager from the given config.
func NewManager(logf logger.Logf, oscfg OSConfigurator, linkMon *monitor.Mon) *Manager {
	logf = logger.WithPrefix(logf, "dns: ")
	m := &Manager{
		logf:     logf,
		resolver: resolver.New(logf, linkMon),
		os:       oscfg,
	}

	m.logf("using %T", m.os)
	return m
}

// forceSplitDNSForTesting alters cfg to be a split DNS configuration
// that only captures search paths. It's intended for testing split
// DNS until the functionality is linked up in the admin panel.
func forceSplitDNSForTesting(cfg *Config) {
	if len(cfg.DefaultResolvers) == 0 {
		return
	}

	if cfg.Routes == nil {
		cfg.Routes = map[string][]netaddr.IPPort{}
	}
	for _, search := range cfg.SearchDomains {
		cfg.Routes[search] = cfg.DefaultResolvers
	}
	cfg.DefaultResolvers = nil
}

func (m *Manager) Set(cfg Config) error {
	m.logf("Set: %+v", cfg)

	if false {
		// Temporary, for danderson to test things.
		forceSplitDNSForTesting(&cfg)
	}

	rcfg, ocfg := m.compileConfig(cfg)

	m.logf("Resolvercfg: %+v", rcfg)
	m.logf("OScfg: %+v", ocfg)

	if err := m.resolver.SetConfig(rcfg); err != nil {
		return err
	}
	if err := m.os.SetDNS(ocfg); err != nil {
		return err
	}

	return nil
}

// compileConfig converts cfg into a quad-100 resolver configuration
// and an OS-level configuration.
func (m *Manager) compileConfig(cfg Config) (resolver.Config, OSConfig) {
	// Deal with trivial configs first.
	switch {
	case !cfg.needsOSResolver():
		// Set search domains, but nothing else. This also covers the
		// case where cfg is entirely zero, in which case these
		// configs clear all Tailscale DNS settings.
		return resolver.Config{}, OSConfig{
			SearchDomains: cfg.SearchDomains,
		}
	case cfg.hasDefaultResolversOnly():
		// Trivial CorpDNS configuration, just override the OS
		// resolver.
		return resolver.Config{}, OSConfig{
			Nameservers:   toIPsOnly(cfg.DefaultResolvers),
			SearchDomains: cfg.SearchDomains,
		}
	case cfg.hasDefaultResolvers():
		// Default resolvers plus other stuff always ends up proxying
		// through quad-100.
		rcfg := resolver.Config{
			Routes: map[string][]netaddr.IPPort{
				".": cfg.DefaultResolvers,
			},
			Hosts:        cfg.Hosts,
			LocalDomains: addFQDNDots(cfg.AuthoritativeSuffixes),
		}
		for suffix, resolvers := range cfg.Routes {
			rcfg.Routes[suffix+"."] = resolvers
		}
		ocfg := OSConfig{
			Nameservers:   []netaddr.IP{tsaddr.TailscaleServiceIP()},
			SearchDomains: cfg.SearchDomains,
		}
		return rcfg, ocfg
	}

	// From this point on, we're figuring out split DNS
	// configurations. The possible cases don't return directly any
	// more, because as a final step we have to handle the case where
	// the OS can't do split DNS.
	var rcfg resolver.Config
	var ocfg OSConfig

	if !cfg.hasHosts() && cfg.singleResolverSet() != nil && m.os.SupportsSplitDNS() {
		// Split DNS configuration requested, where all split domains
		// go to the same resolvers. We can let the OS do it.
		return resolver.Config{}, OSConfig{
			Nameservers:   toIPsOnly(cfg.singleResolverSet()),
			SearchDomains: cfg.SearchDomains,
			MatchDomains:  cfg.matchDomains(),
		}
	}

	// Split DNS configuration with either multiple upstream routes,
	// or routes + MagicDNS, or just MagicDNS, or on an OS that cannot
	// split-DNS. Install a split config pointing at quad-100.
	rcfg = resolver.Config{
		Routes:       map[string][]netaddr.IPPort{},
		Hosts:        cfg.Hosts,
		LocalDomains: addFQDNDots(cfg.AuthoritativeSuffixes),
	}
	for suffix, resolvers := range cfg.Routes {
		rcfg.Routes[suffix+"."] = resolvers
	}
	ocfg = OSConfig{
		Nameservers:   []netaddr.IP{tsaddr.TailscaleServiceIP()},
		SearchDomains: cfg.SearchDomains,
	}

	// If the OS can't do native split-dns, read out the underlying
	// resolver config and blend it into our config.
	// TODO: for now, use quad-8 as the upstream until more plumbing
	// is done.
	if m.os.SupportsSplitDNS() {
		ocfg.MatchDomains = cfg.matchDomains()
	} else {
		rcfg.Routes["."] = []netaddr.IPPort{netaddr.MustParseIPPort("8.8.8.8:53")}
	}

	return rcfg, ocfg
}

func addFQDNDots(domains []string) []string {
	ret := make([]string, 0, len(domains))
	for _, dom := range domains {
		ret = append(ret, strings.TrimSuffix(dom, ".")+".")
	}
	return ret
}

// toIPsOnly returns only the IP portion of ipps.
// TODO: this discards port information on the assumption that we're
// always pointing at port 53.
// https://github.com/tailscale/tailscale/issues/1666 tracks making
// that not true, if we ever want to.
func toIPsOnly(ipps []netaddr.IPPort) (ret []netaddr.IP) {
	for _, ipp := range ipps {
		ret = append(ret, ipp.IP)
	}
	return ret
}

func (m *Manager) EnqueueRequest(bs []byte, from netaddr.IPPort) error {
	return m.resolver.EnqueueRequest(bs, from)
}

func (m *Manager) NextResponse() ([]byte, netaddr.IPPort, error) {
	return m.resolver.NextResponse()
}

func (m *Manager) Down() error {
	if err := m.os.Close(); err != nil {
		return err
	}
	m.resolver.Close()
	return nil
}

// Cleanup restores the system DNS configuration to its original state
// in case the Tailscale daemon terminated without closing the router.
// No other state needs to be instantiated before this runs.
func Cleanup(logf logger.Logf, interfaceName string) {
	oscfg := NewOSConfigurator(logf, interfaceName)
	dns := NewManager(logf, oscfg, nil)
	if err := dns.Down(); err != nil {
		logf("dns down: %v", err)
	}
}
