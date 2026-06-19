package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

type stateTarget struct {
	StateName string
	BaseURL   string
	Username  string
	Password  string
}

func resolveStateTarget(positional, cwd string) (target stateTarget, discovered bool, err error) {
	if parsed, ok, err := stateTargetFromAddress(strings.TrimSpace(positional)); ok || err != nil {
		return parsed, false, err
	}
	if positional != "" {
		return stateTarget{StateName: strings.TrimSpace(positional)}, false, nil
	}
	if envAddr := strings.TrimSpace(os.Getenv("KL_STATE_URL")); envAddr != "" {
		parsed, _, err := stateTargetFromAddress(envAddr)
		if err != nil {
			return stateTarget{}, false, fmt.Errorf("parse KL_STATE_URL: %w", err)
		}
		return parsed, true, nil
	}
	bi, err := discoverLiveBackend(cwd)
	if err != nil {
		return stateTarget{}, false, fmt.Errorf("--state name required (no state URL provided and no http backend discovered in %s: %v)", cwd, err)
	}
	base, err := baseURLFromAddress(bi.Address)
	if err != nil {
		return stateTarget{}, false, fmt.Errorf("derive backend base URL from %q: %w", bi.Address, err)
	}
	return stateTarget{
		StateName: bi.StateName,
		BaseURL:   base,
		Username:  bi.Username,
		Password:  bi.Password,
	}, true, nil
}

func stateTargetFromAddress(addr string) (target stateTarget, ok bool, err error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return stateTarget{}, false, nil
	}
	u, err := url.Parse(addr)
	if err != nil {
		return stateTarget{}, false, fmt.Errorf("parse state URL %q: %w", addr, err)
	}
	if strings.TrimSpace(u.Scheme) == "" || strings.TrimSpace(u.Host) == "" {
		return stateTarget{}, false, nil
	}
	stateName, err := stateNameFromAddress(addr)
	if err != nil {
		return stateTarget{}, true, err
	}
	base, err := baseURLFromAddress(addr)
	if err != nil {
		return stateTarget{}, true, err
	}
	target = stateTarget{
		StateName: stateName,
		BaseURL:   base,
	}
	if u.User != nil {
		target.Username = u.User.Username()
		target.Password, _ = u.User.Password()
	}
	return target, true, nil
}

func stateNameFromAddress(addr string) (string, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	p := strings.TrimRight(u.Path, "/")
	if p == "" || p == "/" {
		return "", fmt.Errorf("address has no path segment to use as state name")
	}
	if i := strings.Index(p, "/states/"); i >= 0 {
		name := strings.Trim(strings.TrimPrefix(p[i:], "/states/"), "/")
		if name == "" {
			return "", fmt.Errorf("address path %q ends without a state name segment", u.Path)
		}
		return name, nil
	}
	i := strings.LastIndex(p, "/")
	name := p[i+1:]
	if name == "" {
		return "", fmt.Errorf("address path %q ends without a state name segment", u.Path)
	}
	return name, nil
}

func baseURLFromAddress(addr string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(addr))
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	if strings.TrimSpace(u.Scheme) == "" || strings.TrimSpace(u.Host) == "" {
		return "", fmt.Errorf("address %q is not an absolute URL", addr)
	}
	return strings.TrimRight((&url.URL{Scheme: u.Scheme, Host: u.Host}).String(), "/"), nil
}
