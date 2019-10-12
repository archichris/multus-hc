// Copyright 2015 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	// "encoding/json"
	// "flag"
	"fmt"
	"net"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ip"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
	"github.com/intel/multus-cni/logging"
	"github.com/intel/multus-cni/multus-ipam/backend/allocator"
	"github.com/intel/multus-cni/multus-ipam/backend/disk"
	"github.com/intel/multus-cni/multus-ipam/backend/etcdv3"
)

const defaultApplyUnit = uint32(16)

func init() {
	//for debug
	logging.SetLogFile("/tmp/multus-ipam.log")
	logging.SetLogLevel("debug")
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, bv.BuildString("multus-ipam"))
}

func cmdCheck(args *skel.CmdArgs) error {

	ipamConf, _, err := allocator.LoadIPAMConfig(args.StdinData, args.Args)
	if err != nil {
		return err
	}

	// Look to see if there is at least one IP address allocated to the container
	// in the data dir, irrespective of what that address actually is
	store, err := disk.New(ipamConf.Name, ipamConf.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()

	containerIPFound := store.FindByID(args.ContainerID, args.IfName)
	if containerIPFound == false {
		return fmt.Errorf("host-local: Failed to find address added by container %v", args.ContainerID)
	}

	return nil
}

func cmdAdd(args *skel.CmdArgs) error {
	ipamConf, confVersion, err := allocator.LoadIPAMConfig(args.StdinData, args.Args)
	logging.Debugf("%v", args)
	if err != nil {
		return logging.Errorf("LoadIPAMConfig failed, %v", err)
	}

	result := &current.Result{}

	if ipamConf.ResolvConf != "" {
		logging.Debugf("ipamConf.ResolvConf=%v", ipamConf.ResolvConf)
		dns, err := parseResolvConf(ipamConf.ResolvConf)
		if err != nil {
			return logging.Errorf("parseResolvConf failed, %v", err)
		}
		result.DNS = *dns
	}

	if ipamConf.ApplyUnit == 0 {
		ipamConf.ApplyUnit = defaultApplyUnit
	}
	logging.Debugf("ipamConf.ApplyUnit=%v", ipamConf.ApplyUnit)

	store, err := disk.New(ipamConf.Name, ipamConf.DataDir)
	if err != nil {
		return logging.Errorf("disk.New(%v, %v) failed, %v", ipamConf.Name, ipamConf.DataDir, err)
	}
	defer store.Close()

	result.IPs, err = allocateIP(ipamConf, store, args.ContainerID, args.IfName)
	if err != nil {
		return logging.Errorf("allocateIP failed, %v", err)
	}

	result.Routes = ipamConf.Routes

	// s = fmt.Sprintf("result:%+v\n", result)
	// f.WriteString(s)

	return types.PrintResult(result, confVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	ipamConf, _, err := allocator.LoadIPAMConfig(args.StdinData, args.Args)
	if err != nil {
		return err
	}

	store, err := disk.New(ipamConf.Name, ipamConf.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()

	// Loop through all ranges, releasing all IPs, even if an error occurs
	var errors []string
	for idx, rangeset := range ipamConf.Ranges {
		ipAllocator := allocator.NewIPAllocator(&rangeset, store, idx)

		err := ipAllocator.Release(args.ContainerID, args.IfName)
		if err != nil {
			errors = append(errors, err.Error())
		}
	}

	if errors != nil {
		return fmt.Errorf(strings.Join(errors, ";"))
	}
	return nil
}

func formRangeSets(origin []allocator.RangeSet, network string, unit uint32, store *disk.Store) ([]allocator.RangeSet, error) {
	// load IP range set from local cache, "IPStart-IPEnd"
	c, err := store.LoadRangeSetFromCache()
	if err != nil {
		return nil, err
	}

	logging.Debugf("Origin: %v", origin)

	// parse cache to net.IP format
	var cacheRangeSet []allocator.SimpleRange
	for _, r := range c {
		pairIP := strings.Split(r, "-")
		cacheRangeSet = append(cacheRangeSet, allocator.SimpleRange{net.ParseIP(pairIP[0]), net.ParseIP(pairIP[1])})
	}
	logging.Debugf("Cache: %v", cacheRangeSet)

	// RangeSets to find
	rss := []allocator.RangeSet{}
	for _, rso := range origin {
		rs := allocator.RangeSet{}
		for _, ro := range rso {
			for _, cr := range cacheRangeSet {
				if ro.Contains(cr.RangeStart) || ro.Contains(cr.RangeEnd) {
					r := ro
					if ip.Cmp(ro.RangeStart, cr.RangeStart) < 0 {
						r.RangeStart = cr.RangeStart
					}
					if ip.Cmp(ro.RangeEnd, cr.RangeEnd) > 0 {
						r.RangeEnd = cr.RangeEnd
					}
					rs = append(rs, r)
				}

			}
		}
		rss = append(rss, rs)
	}
	logging.Debugf("Rangesets: %v", rss)
	return rss, nil
}

func allocateIP(ipamConf *allocator.IPAMConfig, store *disk.Store, containerID string, ifName string) ([]*current.IPConfig, error) {

	// genereate the ip ranges that can be allocated locally
	rss, err := formRangeSets(ipamConf.Ranges, ipamConf.Name, ipamConf.ApplyUnit, store)
	if err != nil {
		return nil, err
	}
	logging.Debugf("allocate ip from %v", rss)
	allocs := []*allocator.IPAllocator{}
	IPs := []*current.IPConfig{}
	for idx, rs := range rss {
		var err error = nil
		var ipConf *current.IPConfig = nil
		var alloc *allocator.IPAllocator = nil
		if len(rs) > 0 {
			alloc = allocator.NewIPAllocator(&rs, store, idx)
			logging.Debugf("allocator(%v, %v, %v) return v%", rs, store, idx, alloc)
			ipConf, err = alloc.Get(containerID, ifName, nil)
		} else {
			err = logging.Errorf("no IP addresses available in range set")
		}
		//try most 3 times
		for i := 0; i < 3; i++ {
			if err != nil && strings.Contains(err.Error(), "no IP addresses available in range set") {
				// apply IP slice from etcd if there is no available IP addresses
				// todo use whole origin rangeset to apply ip pool
				var sIP, eIP net.IP
				sIP, eIP, err = etcdv3.ApplyNewIPRange(ipamConf.Name, &ipamConf.Ranges[idx][0].Subnet, ipamConf.ApplyUnit)
				logging.Debugf("apply new ip range(%v, %v, %v) return %v, %v, %v", ipamConf.Name, &ipamConf.Ranges[idx][0].Subnet, ipamConf.ApplyUnit, sIP, eIP, err)
				if err == nil {
					store.AppendRangeToCache(fmt.Sprintf("%s-%s", sIP.String(), eIP.String()))
					r := ipamConf.Ranges[idx][0]
					r.RangeStart, r.RangeEnd = sIP, eIP
					alloc = allocator.NewIPAllocator(&(allocator.RangeSet{r}), store, idx)
					logging.Debugf("NewIPAllocator(%v, %v, %v) return v%", allocator.RangeSet{r}, store, idx, alloc)
					ipConf, err = alloc.Get(containerID, ifName, nil)
					if err != nil {
						logging.Errorf("alloc ip from range %v failed, %v", r, err)
						continue
					}
				}
			}
			break
		}
		if err != nil {
			// Deallocate all already allocated IPs
			for _, alloc := range allocs {
				_ = alloc.Release(containerID, ifName)
			}
			return nil, logging.Errorf("failed to allocate for range %d: %v", idx, err)
		}
		allocs = append(allocs, alloc)
		IPs = append(IPs, ipConf)
	}

	logging.Debugf("Return IPS: %v", IPs)
	return IPs, nil
}
