package proxmox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

const maxSSHPublicKeys = 16 << 10

// validBaseTemplate проверяет форму volid storage:vztmpl/file без "..".
func validBaseTemplate(volid string) bool {
	storage, file, ok := strings.Cut(volid, ":vztmpl/")
	return ok && len(volid) <= 255 && name.MatchString(storage) && templateFile.MatchString(file) && !strings.Contains(file, "..")
}

// authorizedKeys нормализует строки ключей: по одному ключу на строку, без
// опций и управляющих символов. Пустой ввод — пустой результат.
func authorizedKeys(keys string) (string, error) {
	fail := errors.New("invalid PVE container SSH public keys")
	if keys == "" {
		return "", nil
	}
	if len(keys) > maxSSHPublicKeys {
		return "", fail
	}
	var lines []string
	for _, line := range strings.Split(keys, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, r := range line {
			if r < 32 || r == 127 {
				return "", fail
			}
		}
		_, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
			return "", fail
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "", fail
	}
	return strings.Join(lines, "\n"), nil
}

// Clone submits one linked clone of an owned template container and returns the
// task UPID. Only VMID, Hostname and Owner of target are used; the clone joins
// the configured resource pool and carries the target ownership marker. The
// source must pass the full ownership check and be a PVE template. As for
// Create, an uncertain result must be reconciled, never blindly retried.
func (c *Client) Clone(ctx context.Context, source OwnedCT, target CTSpec) (string, error) {
	if !validVMID(target.VMID) || target.VMID == source.VMID || !target.Owner.valid() || !hostname.MatchString(target.Hostname) || len(target.Hostname) > 253 {
		return "", errors.New("invalid PVE container specification")
	}
	cfg, err := c.ownedConfig(ctx, source, false)
	if err != nil {
		return "", err
	}
	if !cfg.Template {
		return "", errors.New("PVE clone source is not a template")
	}
	form := url.Values{"newid": {strconv.Itoa(target.VMID)}, "hostname": {target.Hostname}, "description": {OwnedDescription(target.Owner)}, "pool": {c.cfg.ResourcePool}}
	return c.writeTask(ctx, http.MethodPost, c.ctPath(source.VMID)+"/clone", form)
}

// ConvertToTemplate turns a stopped, fully owned container into a PVE template.
// An owned container that already is a template is left untouched. The
// conversion cannot be undone through this client. Errors after submission are
// uncertain; callers reconcile with InspectOwned before retrying.
func (c *Client) ConvertToTemplate(ctx context.Context, ref OwnedCT) error {
	cfg, err := c.ownedConfig(ctx, ref, false)
	if err != nil {
		return err
	}
	if cfg.Template {
		return nil
	}
	status, err := c.Status(ctx, ref.VMID)
	if err != nil {
		return err
	}
	if status.Status != "stopped" {
		return errors.New("PVE container must be stopped before conversion to a template")
	}
	var result json.RawMessage
	err = c.requestData(ctx, http.MethodPost, c.ctPath(ref.VMID)+"/template", url.Values{}, &result, true)
	if err != nil {
		if errors.Is(err, ErrPermission) || errors.Is(err, ErrCollision) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrUncertain, err)
	}
	if string(bytes.TrimSpace(result)) == "null" {
		return nil
	}
	// Некоторые версии PVE выполняют преобразование задачей и отдают UPID.
	var upid string
	if json.Unmarshal(result, &upid) != nil || !c.validTask(upid) {
		return fmt.Errorf("%w: invalid PVE template conversion result", ErrUncertain)
	}
	return c.WaitTask(ctx, upid)
}

// Resize sets cores and memory of a fully owned container and keeps swap at 0.
// It uses PVE's digest compare-and-set like ConfigureBootstrap and accepts the
// same bounds as Create: 1..128 cores and 16..1048576 MiB. PVE applies the
// values to a running container where it can. No automatic retries occur.
func (c *Client) Resize(ctx context.Context, ref OwnedCT, cores, memoryMiB int) error {
	if cores < 1 || cores > 128 || memoryMiB < 16 || memoryMiB > 1048576 {
		return errors.New("invalid PVE container specification")
	}
	digest, err := c.ownedDigest(ctx, ref, true)
	if err != nil {
		return err
	}
	var result json.RawMessage
	form := url.Values{"cores": {strconv.Itoa(cores)}, "memory": {strconv.Itoa(memoryMiB)}, "swap": {"0"}, "digest": {digest}}
	err = c.requestData(ctx, http.MethodPut, c.ctPath(ref.VMID)+"/config", form, &result, true)
	if err != nil {
		if errors.Is(err, ErrPermission) || errors.Is(err, ErrCollision) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrUncertain, err)
	}
	if string(bytes.TrimSpace(result)) != "null" {
		return fmt.Errorf("%w: invalid PVE config update result", ErrUncertain)
	}
	return nil
}

// Addresses lists the addresses PVE reports for the eth0 interface of a running
// container, IPv4 first. Loopback, link-local, unspecified and multicast
// addresses are skipped. An empty result is not an error: DHCP may not have
// answered yet. It performs no ownership check and proves no guest readiness.
func (c *Client) Addresses(ctx context.Context, id int) ([]netip.Addr, error) {
	if !validVMID(id) {
		return nil, errors.New("invalid PVE VMID")
	}
	// prefix приходит то числом, то строкой и здесь не нужен: поле не объявлено.
	var interfaces []struct {
		Name  string `json:"name"`
		Inet  string `json:"inet"`
		Inet6 string `json:"inet6"`
		IPs   []struct {
			Address string `json:"ip-address"`
		} `json:"ip-addresses"`
	}
	if err := c.requestData(ctx, http.MethodGet, c.ctPath(id)+"/interfaces", nil, &interfaces, true); err != nil {
		return nil, err
	}
	var out []netip.Addr
	seen := make(map[netip.Addr]bool)
	add := func(raw string) {
		raw, _, _ = strings.Cut(raw, "/")
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return
		}
		addr = addr.Unmap().WithZone("")
		if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() || addr.IsMulticast() || seen[addr] {
			return
		}
		seen[addr] = true
		out = append(out, addr)
	}
	for _, iface := range interfaces {
		if iface.Name != "eth0" {
			continue
		}
		add(iface.Inet)
		add(iface.Inet6)
		for _, ip := range iface.IPs {
			add(ip.Address)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Is4() && !out[j].Is4() })
	return out, nil
}
