package etcdv3cli

import (
	"context"
	"path/filepath"
	"math"

	"fmt"
	"net"

	"strings"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/coreos/etcd/clientv3"

	"github.com/intel/multus-cni/etcdv3"
	"github.com/intel/multus-cni/multus-ipam/backend/disk"
	"github.com/intel/multus-cni/ipaddr"
	"github.com/intel/multus-cni/logging"
	"github.com/intel/multus-cni/multus-ipam/backend/allocator"
)

var (
	leaseDir    = "lease" //multus/netowrkname/key(ipsegment):value(node)
	staticDir   = "static"
	keyTemplate = "%010d-%d"
	maxApplyTry = 3
)

func ipmaLeaseToUint32Range(key string) (IPStart uint32, IPEnd uint32) {
	lease := strings.Split(filepath.Base(key), "-")
	IPStart = ipaddr.StrToUint32(lease[0])
	hostSize := ipaddr.StrToUint32(lease[1])
	IPEnd = ipaddr.Uint32AddSeg(IPStart, hostSize) - 1
	return IPStart, IPEnd
}

func ipamLeaseToSimleRange(l string) *allocator.SimpleRange {
	ips, ipe := ipmaLeaseToUint32Range(l)
	return &allocator.SimpleRange{ipaddr.Uint32ToIP4(ips), ipaddr.Uint32ToIP4(ipe)}
}

func ipamSimpleRangeToLease(keyDir string, rs *allocator.SimpleRange) string {
	ips := ipaddr.IP4ToUint32(rs.RangeStart)
	n := rs.HostSize()
	return filepath.Join(keyDir, fmt.Sprintf(keyTemplate, ips, n))
}

// IpamApplyIPRange is used to apply IP range from ectd
func IPAMApplyIPRange(netConf *allocator.Net, subnet *types.IPNet) (*allocator.SimpleRange, error) {
	logging.Debugf("Going to do apply IP range from %v", subnet)
	etcdMultus, err := etcdv3.New()
	if err != nil {
		return nil, err
	}
	cli, rKeyDir, id := etcdMultus.Cli,etcdMultus.RootKeyDir, etcdMultus.Id
	defer cli.Close() // make sure to close the client

	keyDir := filepath.Join(rKeyDir, leaseDir, netConf.Name)

	rs, err := ipamGetFreeIPRange(cli, keyDir, subnet, netConf.IPAM.ApplyUnit)
	if err != nil {
		return nil, err
	}
	err = etcdv3.TransPutKey(cli, ipamSimpleRangeToLease(keyDir, rs), id, true)
	if err != nil {
		return nil, logging.Errorf("write key %v with value %v failed", ipamSimpleRangeToLease(keyDir, rs), id)
	}

	return rs, nil
}

// GetFreeIPRange is used to find a free IP range
func ipamGetFreeIPRange(cli *clientv3.Client, keyDir string, subnet *types.IPNet, n uint32) (*allocator.SimpleRange, error) {
	unit := uint32(math.Pow(2, float64(n)))
	logging.Debugf("ipamGetFreeIPRange(%v,%v,%v)", keyDir, *subnet, unit)
	rips, ripe := ipaddr.Net4To2Uint32((*net.IPNet)(subnet))
	last := ripe
	ctx, cancel := context.WithTimeout(context.Background(), etcdv3.RequestTimeout)
	resp, err := cli.Get(ctx, keyDir, clientv3.WithPrefix(), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
	cancel()
	if err != nil {
		return nil, logging.Errorf("Get %v failed, %v", keyDir, err)
	}
	var sips, sipe uint32
	for _, ev := range resp.Kvs {
		logging.Debugf("Key:%v, Value:%v ", string(ev.Key), string(ev.Value))
		ips, ipe := ipmaLeaseToUint32Range(string(ev.Key))
		if ips == 0 {
			logging.Debugf("Invalid Key %v", string(ev.Key))
			continue
		}
		if ips-last < unit {
			last = ipe + 1
			continue
		}
		sips = last
		sipe = last + unit - 1
		logging.Debugf("get IP range (%v-%v) from (%v-%v) mode 1", sips, sipe, rips, ripe)
		return &allocator.SimpleRange{ipaddr.Uint32ToIP4(sips), ipaddr.Uint32ToIP4(sipe)}, nil
	}
	return nil, logging.Errorf("apply ip range failed")
}

func IPAMGetAllLease(cli *clientv3.Client, keyDir, id string) (map[string][]allocator.SimpleRange, error) {
	logging.Debugf("Going to get all IP lease belong to %v from %v", id, keyDir)
	ctx, cancel := context.WithTimeout(context.Background(), etcdv3.RequestTimeout)
	resp, err := cli.Get(ctx, keyDir, clientv3.WithPrefix(), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
	cancel()
	if err != nil {
		return nil, logging.Errorf("Get %v failed, %v", keyDir, err)
	}
	leases := make(map[string][]allocator.SimpleRange)
	for _, ev := range resp.Kvs {
		v := strings.Trim(string(ev.Value), " \r\n\t")
		logging.Debugf("Key:%v, Value:%v, id:%v, match:%v ", string(ev.Key), v, id, v == id)
		if v == id {
			k := strings.Trim(string(ev.Key), " \r\n\t")
			network := filepath.Base(filepath.Dir(k))
			sr := ipamLeaseToSimleRange(k)
			if _, ok := leases[network]; ok {
				leases[network] = append(leases[network], *sr)
			} else { 
				leases[network] = []allocator.SimpleRange{*sr}
			}
		}
	}
	return leases, nil
}

func ipamCheckNet(em *etcdv3.EtcdMultus, network string, leases []allocator.SimpleRange) {
	s, err:= disk.New(network, "")
	if err != nil{
		logging.Errorf("create disk manager failed, %v", err)
		return 
	}
	caches, err := s.LoadCache()
	if err != nil{
		logging.Errorf("get cache failed, %v", err)
		return 
	}
	keyDir := filepath.Join(em.RootKeyDir,leaseDir, network)
	cli, id:= em.Cli, em.Id
	var last *allocator.SimpleRange
	for _, lsr := range leases {
		last = nil
		for _, csr := range caches {
			if csr.Overlaps(&lsr) {
				if csr.Match(&lsr) {
					last = &csr
					break
				} else {
					// caches = delete(caches, csr)
					s.DeleteCache(&csr)
				}
			}
		}
		if last == nil {
			err := s.AppendCache(&lsr)
			if err != nil {
				etcdv3.TransDelKey(cli, ipamSimpleRangeToLease(keyDir, &lsr))
			} 
		}
	}

	caches, err = s.LoadCache()
	if err != nil{
		logging.Errorf("get cache failed, %v", err)
		return 
	}	
	for _, csr := range caches {
		last = nil
		for _, lsr := range leases {
			if csr.Match(&lsr) {
				last = &csr
				break
			}
		}
		if last == nil {
			err = etcdv3.TransPutKey(cli, ipamSimpleRangeToLease(keyDir, &csr), id, true)
			if err != nil {
				s.DeleteCache(&csr)
			}
		}
	}
}

func IPAMCheck() error {
	logging.Debugf("Going to check IPAM")
	etcdMultus, err := etcdv3.New()
	cli, rKeyDir, id := etcdMultus.Cli, etcdMultus.RootKeyDir, etcdMultus.Id
	if err != nil {
		return err
	}
	defer cli.Close() // make sure to close the client

	leaseDir = filepath.Join(rKeyDir, leaseDir)

	leases, err := IPAMGetAllLease(cli, leaseDir, id)
	if err != nil {
		return err
	}
	if len(leases) == 0 {
		logging.Debugf("No node lease found")
		return nil
	}

	for n, l := range leases {
		ipamCheckNet(etcdMultus, n, l)
	}
	return nil
}
