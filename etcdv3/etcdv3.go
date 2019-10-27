package etcdv3

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"time"

	"path/filepath"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/clientv3/concurrency"
	"github.com/coreos/etcd/pkg/transport"
	"github.com/intel/multus-cni/logging"
)

var (
	RequestTimeout = 5 * time.Second
)

var (
	dialTimeout        = 5 * time.Second
	defaultEtcdCfgDir  = "/etc/cni/net.d/multus.d/etcd"
	defaultEtcdRootDir = "multus"
	defaultEtcdCfgName = "etcd.conf"
)

// etcdCfg is the struct of stored etcd information
type etcdCfg struct {
	Name      string   `json:"name"`
	Endpoints []string `json:"endpoints"`
	Auth      authCfg  `json:"auth"`
}

type authCfg struct {
	Client authClient `json:"client"`
	Peer   authPeer   `json:"peer"`
}

type authClient struct {
	SecureTransport      bool   `json:"secureTransport"`
	EnableAuthentication bool   `json:"enableAuthentication"`
	SecretDirectory      string `json:"secretDirectory"`
}

type authPeer struct {
	SecureTransport      bool `json:"secureTransport"`
	EnableAuthentication bool `json:"enableAuthentication"`
	UseAutoTLS           bool `json:"useAutoTLS"`
}

type EtcdMultus struct {
	Cli *clientv3.Client
	RootKeyDir string
	Id string
}

func getInitParams() (etcdCfgDir string, rootKeyDir string, id string) {
	etcdCfgDir = os.Getenv("ETCD_CFG_DIR")
	if etcdCfgDir == "" {
		logging.Verbosef("using default etcd cfg dir: %s ", defaultEtcdCfgDir)
		etcdCfgDir = defaultEtcdCfgDir
	}
	etcdCfgDir = strings.Trim(etcdCfgDir, " \r\n\t")

	rootKeyDir = os.Getenv("ETCD_ROOT_DIR")
	if rootKeyDir == "" {
		logging.Verbosef("using default etcd root key dir: %s ", defaultEtcdCfgDir)
		rootKeyDir = defaultEtcdRootDir
	}
	rootKeyDir = strings.Trim(rootKeyDir, " \r\n\t")

	id = os.Getenv("HOSTNAME")
	if id == "" {
		logging.Verbosef("using id from file %s", filepath.Join(etcdCfgDir, "id"))
		data, err := ioutil.ReadFile(filepath.Join(etcdCfgDir, "id"))
		if err == nil {
			id = string(data)
		} else {
			logging.Errorf("can not get id from %s", filepath.Join(etcdCfgDir, "id"))
		}
	}
	id = strings.Trim(id, " \r\n\t")
	return etcdCfgDir, rootKeyDir, id
}

func getEtcdCfg(cfg string) (*etcdCfg, error) {
	data, err := ioutil.ReadFile(cfg)
	if err != nil {
		return nil, logging.Errorf("can not get etcd config from %v", cfg)
	}
	var etcdCfg etcdCfg
	err = json.Unmarshal(data, &etcdCfg)
	if err != nil {
		return nil, logging.Errorf("etcd config is not right, %v", err)
	}

	if len(etcdCfg.Endpoints) == 0 {
		return nil, logging.Errorf("no etcd endpoints")
	}

	return &etcdCfg, nil
}

//NewClient Create a new etcd client, and provide a unify id  for node
func New() (*EtcdMultus, error) {
	etcdCfgDir, rootKeyDir, id := getInitParams()
	logging.Debugf("using parameters: etcdCfgDir:%v, rootKeyDir:%v, id:%v", etcdCfgDir, rootKeyDir, id)

	etcdCfg, err := getEtcdCfg(filepath.Join(etcdCfgDir, defaultEtcdCfgName))
	if err != nil {
		return nil, err
	}

	var cli *clientv3.Client

	if etcdCfg.Auth.Client.SecureTransport {
		logging.Debugf("using secure transport")
		tlsInfo := transport.TLSInfo{
			CertFile:      etcdCfg.Auth.Client.SecretDirectory + "/etcd-client.crt",
			KeyFile:       etcdCfg.Auth.Client.SecretDirectory + "/etcd-client.key",
			TrustedCAFile: etcdCfg.Auth.Client.SecretDirectory + "/etcd-client-ca.crt",
		}
		tlsConfig, err := tlsInfo.ClientConfig()
		if err != nil {
			return nil, logging.Errorf("create tls config failed, %v", err)
		}
		cli, err = clientv3.New(clientv3.Config{
			Endpoints:   etcdCfg.Endpoints,
			DialTimeout: dialTimeout,
			TLS:         tlsConfig,
		})
		if err != nil {
			return nil, logging.Errorf("create etcd client failed, %v", err)
		}
	} else {
		logging.Debugf("using plain transport, %v", etcdCfg.Endpoints)
		cli, err = clientv3.New(clientv3.Config{
			Endpoints:   etcdCfg.Endpoints,
			DialTimeout: dialTimeout,
		})
		if err != nil {
			log.Println(err)
			return nil, logging.Errorf("create etcd client failed, %v", err)
		}
	}
	return &EtcdMultus{cli, rootKeyDir, id}, nil
}
func (e *EtcdMultus)Close(){
    e.Cli.Close()
}

func KeyToMutex(key string) string {
	ss := strings.Split(filepath.Dir(key), "/")
	mutex := filepath.Join(ss[0], "mutex")
	for _, s := range ss[1:] {
		mutex = filepath.Join(mutex, s)
	}
	logging.Debugf("key:%v,mutex:%v", key, mutex)
	return mutex
}

func TransPutKey(c *clientv3.Client, key string, value string, noExist bool) error {
	cli := c
	if cli == nil {
		var err error
		etcdMultus, err := New()
		if err != nil {
			return logging.Errorf("Create etcd client failed, %v", err)
		}
		cli = etcdMultus.Cli
		defer cli.Close()
	}

	s, err := concurrency.NewSession(cli)
	if err != nil {
		return logging.Errorf("create etcd session failed, %v", err)
	}
	defer s.Close()

	mutex := KeyToMutex(key)
	m := concurrency.NewMutex(s, mutex)

	if err := m.Lock(context.TODO()); err != nil {
		return logging.Errorf("get etcd locd failed, %v", err)
	}

	defer func() {
		if err := m.Unlock(context.TODO()); err != nil {
			logging.Debugf("unlock etcd mutex failed, %v", err)
		}
	}()

	if noExist {
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
		resp, err := cli.Get(ctx, key)
		cancel()
		if err != nil {
			return logging.Errorf("failed to check key %v, %v", key, err)
		}
		if len(resp.Kvs) != 0 {
			return logging.Errorf("key %v exists", key)
		}
	}

	_, err = cli.Put(context.TODO(), key, value)
	if err != nil {
		return logging.Errorf("write key %v to %v failed", key, value)
	}

	return nil
}

func TransDelKey(c *clientv3.Client, key string) error {
	cli := c
	if cli == nil {
		var err error
		etcdMultus, err := New()
		if err != nil {
			return logging.Errorf("Create etcd client failed, %v", err)
		}
		cli = etcdMultus.Cli
		defer cli.Close()
	}

	s, err := concurrency.NewSession(cli)
	if err != nil {
		return logging.Errorf("create etcd session failed, %v", err)
	}
	defer s.Close()

	mutex := KeyToMutex(key)
	m := concurrency.NewMutex(s, mutex)

	if err := m.Lock(context.TODO()); err != nil {
		return logging.Errorf("get etcd locd failed, %v", err)
	}

	defer func() {
		if err := m.Unlock(context.TODO()); err != nil {
			logging.Debugf("unlock etcd mutex failed, %v", err)
		}
	}()

	_, err = cli.Delete(context.TODO(), key)
	if err != nil {
		return logging.Errorf("delete key %v failed", key)
	}

	return nil
}

func TransDelKeys(c *clientv3.Client, keys []string) {
	for _, k := range keys {
		TransDelKey(c, k)
	}
}

// func GetWithPrefix(prefix string) []*mvccpb.KeyValue{

// 	ctx, cancel := context.WithTimeout(context.Background(), etcdV3.RequestTimeout)
// 	resp, err := cli.Get(ctx, keyDir, clientv3.WithPrefix(), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
// 	cancel()
// }

// func TransDelKeys(c *clientv3.Client, keys []string) {
// 	for _,k:= range keys{
// 		TransDelKey(c, k)
// 	}
// }

// s, err := concurrency.NewSession(cli)
// if err != nil {
// 	return logging.Errorf("create etcd session failed, %v", err)
// }
// defer s.Close()

// var mutex string = ""
// var m *concurrency.Mutex
// logging.Debugf("going to del %v from etcd", keys)
// for _, key := range keys {
// 	tmp := KeyToMutex(key)
// 	logging.Debugf("old mutex:%v, new mutex:%v, m:%v", mutex, tmp, m)
// 	if mutex != tmp {
// 		if m != nil {
// 			err = m.Unlock(context.TODO())
// 			if err != nil {
// 				logging.Errorf("unlock %v failed, %v", mutex, err)
// 			}
// 		}
// 		mutex = tmp
// 		m = concurrency.NewMutex(s, mutex)
// 		if err := m.Lock(context.TODO()); err != nil {
// 			logging.Errorf("lock %v failed, %v", mutex, err)
// 			mutex = ""
// 			m = nil
// 			continue
// 		}
// 	}
// 	_, err = cli.Delete(context.TODO(), key)
// 	if err != nil {
// 		logging.Errorf("del key %v failed", key)
// 	}
// 	logging.Debugf("Del key %v", key)
// }

// if m != nil {
// 	if m.Unlock(context.TODO()); err != nil {
// 		logging.Errorf("lock %v failed, %v", mutex, err)
// 	}
// }
// return nil
// }
