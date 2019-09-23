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
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"strings"

	"github.com/containernetworking/plugins/plugins/ipam/host-etcd/backend/etcdv3"

	"github.com/archichris/multus-cni/host-etcd/backend/allocator"
	"github.com/archichris/multus-cni/host-etcd/backend/disk"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
)

const defaultApplyUnit = uint32(16)

func init() {
	flag.Set("alsologtostderr", "true")
	flag.Set("log_dir", "/var/log/host-etcd.log")
	flag.Parse()
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, bv.BuildString("host-etcd"))
}

func loadNetConf(bytes []byte) (*types.NetConf, string, error) {
	n := &types.NetConf{}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, "", fmt.Errorf("failed to load netconf: %v", err)
	}

	return n, n.CNIVersion, nil
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
	if err != nil {
		return err
	}

	result := &current.Result{}

	if ipamConf.ResolvConf != "" {
		dns, err := parseResolvConf(ipamConf.ResolvConf)
		if err != nil {
			return err
		}
		result.DNS = *dns
	}

	if ipamConf.ApplyUnit == 0 {
		ipamConf.ApplyUnit = defaultApplyUnit
	}

	store, err := disk.New(ipamConf.Name, ipamConf.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()

	result.IPs, err = allocateIP(ipamConf, store, args.ContainerID, args.IfName)
	if err != nil {
		return err
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

	// glog.Info(c)

	// parse cache to net.IP format
	var cacheRangeSet []allocator.SimpleRange
	for _, r := range c {
		pairIP := strings.Split(r, "-")
		cacheRangeSet = append(cacheRangeSet, allocator.SimpleRange{net.ParseIP(pairIP[0]), net.ParseIP(pairIP[1])})
	}

	flashCache := false
	rss := []allocator.RangeSet{}
	for _, ors := range origin {
		rs := allocator.RangeSet{}
		for _, cr := range cacheRangeSet {
			if or, _ := ors.RangeFor(cr.RangeStart); or != nil {
				r := or
				r.RangeStart, r.RangeEnd = or.RangeStart, or.RangeEnd
				rs = append(rs, *r)
			}
		}

		// no exist range match requested subnet
		if len(rs) == 0 {
			// apply ip slice from etcd
			sIP, eIP, err := etcdv3.ApplyNewIPRange(network, &ors[0].Subnet, unit)
			if err != nil {
				return nil, err
			}
			flashCache = true
			r := ors[0]
			r.RangeStart, r.RangeEnd = sIP, eIP
			rs = append(rs, r)
		}
		rss = append(rss, rs)
	}
	if flashCache == true {
		c := []string{}
		for _, rt := range rss {
			for _, r := range rt {
				c = append(c, r.String())
			}
		}
		store.FlashRangeSetToCache(c)
	}
	return rss, nil
}

func allocateIP(ipamConf *allocator.IPAMConfig, store *disk.Store, containerID string, ifName string) ([]*current.IPConfig, error) {

	// genereate the ip ranges that can be allocated locally
	rss, err := formRangeSets(ipamConf.Ranges, ipamConf.Name, ipamConf.ApplyUnit, store)
	if err != nil {
		return nil, err
	}
	// glog.Info(rss)
	reflashCache := false
	allocs := []*allocator.IPAllocator{}
	IPs := []*current.IPConfig{}
	for idx, rangeset := range rss {
		alloc := allocator.NewIPAllocator(&rangeset, store, idx)
		ipConf, err := alloc.Get(containerID, ifName, nil)
		if err != nil {
			if strings.Contains(err.Error(), "no IP addresses available in range set") {
				// apply IP slice from etcd if there is no available IP addresses
				sIP, eIP, err := etcdv3.ApplyNewIPRange(ipamConf.Name, &rangeset[0].Subnet, ipamConf.ApplyUnit)
				r := rangeset[0]
				r.RangeStart, r.RangeEnd = sIP, eIP
				if err == nil {
					alloc := allocator.NewIPAllocator(&(allocator.RangeSet{r}), store, idx)
					ipConf, err = alloc.Get(containerID, ifName, nil)
					if err == nil {
						rss[idx] = append(rss[idx], r)
						reflashCache = true
					} else {
						// Deallocate all already allocated IPs
						for _, alloc := range allocs {
							_ = alloc.Release(containerID, ifName)
						}
						return nil, fmt.Errorf("failed to allocate for range %d: %v", idx, err)
					}
				} else {
					// Deallocate all already allocated IPs
					for _, alloc := range allocs {
						_ = alloc.Release(containerID, ifName)
					}
					return nil, fmt.Errorf("failed to allocate for range %d: %v", idx, err)
				}

			}
		}
		allocs = append(allocs, alloc)
		IPs = append(IPs, ipConf)
	}
	if reflashCache == true {
		c := []string{}
		for _, rt := range rss {
			for _, r := range rt {
				c = append(c, r.String())
			}
		}
		// glog.Info(c)
		store.FlashRangeSetToCache(c)
	}
	return IPs, nil
}
