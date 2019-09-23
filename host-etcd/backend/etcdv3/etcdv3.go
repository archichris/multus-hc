package etcdv3

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/pkg/transport"
)

var (
	dialTimeout       = 5 * time.Second
	requestTimeout    = 10 * time.Second
	defaultEtcdCfgDir = "/etc/cni/net.d/multus.d/etcd"
	rootKeyDir        = "multus" //multus/netowrkname/key(ipsegment):value(node)
	keyTemplate       = "%10d-%10d"
	maxApplyTry       = 3
)

// EtcdJSONCfg is the struct of stored etcd information
type EtcdJSONCfg struct {
	Name       string  `json:"name"`
	Namespace  string  `json:"namespace"`
	ClientPort string  `json:"clientPort"`
	Replicas   int     `json:"replicas`
	Auth       AuthCfg `json:"auth"`
}

type AuthCfg struct {
	Client AuthClient `json:"client"`
	Peer   AuthPeer   `json:"peer"`
}

type AuthClient struct {
	SecureTransport      bool   `json:"secureTransport"`
	EnableAuthentication bool   `json:"enableAuthentication"`
	SecretDirectory      string `json:"secretDirectory"`
}

type AuthPeer struct {
	SecureTransport      bool `json:"secureTransport"`
	EnableAuthentication bool `json:"enableAuthentication"`
	UseAutoTLS           bool `json:"useAutoTLS"`
}

// ApplyNewIPRange is used to apply IP range from ectd
func ApplyNewIPRange(network string, subnet *types.IPNet, unit uint32) (net.IP, net.IP, error) {

	etcdCfgDir := os.Getenv("ETCD_CFG_DIR")
	if etcdCfgDir == "" {
		etcdCfgDir = defaultEtcdCfgDir
	}
	nodeName := os.Getenv("HOSTNAME")

	data, err := ioutil.ReadFile(etcdCfgDir + "/etcd.conf")
	if err != nil {
		log.Println(err)
		return nil, nil, err
	}
	var etcdCfg EtcdJSONCfg
	err = json.Unmarshal(data, &etcdCfg)
	if err != nil {
		log.Println(err)
		return nil, nil, err
	}

	endpoints := []string{}
	endpointTemplate := etcdCfg.Name + "-%d." + etcdCfg.Name + "." + etcdCfg.Namespace + ".svc" + ":" + etcdCfg.ClientPort
	for i := 0; i < etcdCfg.Replicas; i++ {
		endpoint := fmt.Sprintf(endpointTemplate, i)
		endpoints = append(endpoints, endpoint)
	}

	if len(endpoints) == 0 {
		return nil, nil, fmt.Errorf("no etcd endpoints")
	}

	var cli *clientv3.Client

	if etcdCfg.Auth.Client.SecureTransport {
		tlsInfo := transport.TLSInfo{
			CertFile:      etcdCfg.Auth.Client.SecretDirectory + "/etcd-client.crt",
			KeyFile:       etcdCfg.Auth.Client.SecretDirectory + "/etcd-client.key",
			TrustedCAFile: etcdCfg.Auth.Client.SecretDirectory + "/etcd-client-ca.crt",
		}
		tlsConfig, err := tlsInfo.ClientConfig()
		if err != nil {
			log.Println(err)
			return nil, nil, err
		}
		cli, err = clientv3.New(clientv3.Config{
			Endpoints:   endpoints,
			DialTimeout: dialTimeout,
			TLS:         tlsConfig,
		})
		if err != nil {
			log.Println(err)
			return nil, nil, err
		}
	} else {
		cli, err = clientv3.New(clientv3.Config{
			Endpoints:   endpoints,
			DialTimeout: dialTimeout,
		})
		if err != nil {
			log.Println(err)
			return nil, nil, err
		}
	}

	defer cli.Close() // make sure to close the client

	keyDir := rootKeyDir + "/" + network

	// Get free IP range looply
	for i := 1; i <= maxApplyTry; i++ {
		IPBegin, IPEnd, err := GetFreeIPRange(cli, keyDir, subnet, unit)
		if err != nil {
			return nil, nil, err
		}
		claimKey := fmt.Sprintf(keyTemplate, IPBegin, IPEnd)

		// Claim the ownship of the IP range
		putResp, err := cli.Put(context.TODO(), keyDir+"/"+claimKey, nodeName)
		if err != nil {
			log.Println(err)
			return nil, nil, err
		}
		// Verify the ownship of the IP range
		getResp, err := cli.Get(context.TODO(), keyDir+"/"+claimKey)
		if err != nil {
			log.Println(err)
			return nil, nil, err
		}
		if putResp.Header.Revision == getResp.Header.Revision {
			beginIP := make(net.IP, 4)
			endIP := make(net.IP, 4)
			binary.BigEndian.PutUint32(beginIP, IPBegin)
			binary.BigEndian.PutUint32(endIP, IPEnd)
			return beginIP, endIP, nil
		}
	}
	return nil, nil, errors.New("can't apply free IP range")
}

// GetFreeIPRange is used to find a free IP range
func GetFreeIPRange(cli *clientv3.Client, dir string, subnet *types.IPNet, unit uint32) (uint32, uint32, error) {
	bIP := binary.BigEndian.Uint32(subnet.IP.To4()) + 1
	eIP := bIP + ^binary.BigEndian.Uint32(subnet.Mask) - 1
	lastIP := bIP
	getResp, err := cli.Get(context.TODO(), dir, clientv3.WithPrefix(), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
	if err != nil {
		return 0, 0, nil
	}
	var IPBegin, IPEnd uint32
	for _, ev := range getResp.Kvs {
		IPRange := strings.Split(string(ev.Key), "-")
		tmpU64, _ := strconv.ParseUint(IPRange[0][strings.LastIndex(IPRange[0], "/")+1:], 10, 32)
		IPRangeBegin := uint32(tmpU64)
		if uint32(IPRangeBegin)-lastIP <= 1 {
			tmpU64, _ = strconv.ParseUint(IPRange[1], 10, 32)
			lastIP = uint32(tmpU64)
			continue
		}
		IPBegin = lastIP + 1
		IPEnd = IPRangeBegin - 1
		return IPBegin, IPEnd, nil
	}

	if lastIP < eIP {
		block := eIP - lastIP
		if block > unit {
			block = unit
		}
		IPBegin = lastIP + 1
		IPEnd = lastIP + block
		return IPBegin, IPEnd, nil
	}
	return 0, 0, errors.New("can't apply free IP range")
}

func main() {
	// endpoints := []string{"10.96.232.136:6666"}
	unit := uint32(16)
	// nodeName := "node201"
	_, n, _ := net.ParseCIDR("192.168.56.0/24")
	sIP, eIP, err := ApplyNewIPRange("mac-vlan-1", (*types.IPNet)(n), unit)
	if err == nil {
		fmt.Println(sIP.String() + ":" + eIP.String())
	} else {
		fmt.Println(err)
	}
}
