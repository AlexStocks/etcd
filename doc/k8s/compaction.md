# k8s

## etcd

> etcd-compaction-interval 参数

```go
// staging/src/k8s.io/apiserver/pkg/storage/storagebackend/config.go
const (
	DefaultCompactInterval = 5 * time.Minute
)

func NewDefaultConfig(prefix string, codec runtime.Codec) *Config {
	return &Config{
		Paging:             true,
		Prefix:             prefix,
		Codec:              codec,
		CompactionInterval: DefaultCompactInterval,
	}
}

// staging/src/k8s.io/apiserver/pkg/server/options/etcd.go
func (s *EtcdOptions) AddFlags(fs *pflag.FlagSet) {
	fs.DurationVar(&s.StorageConfig.CompactionInterval, "etcd-compaction-interval", s.StorageConfig.CompactionInterval,
		"The interval of compaction requests. If 0, the compaction request from apiserver is disabled.")
}
```

> etcd 压缩任务

```go
const (
	compactRevKey = "compact_rev_key"
)

var (
	endpointsMapMu sync.Mutex
	endpointsMap   map[string]struct{}
)

func init() {
	endpointsMap = make(map[string]struct{})
}

// StartCompactor 启动一个后台任务，删除不再需要的旧版本数据。一般情况下，只保留最近 10 分钟的数据，足以满足慢 watcher 的需求，且能容忍 burst。
func StartCompactor(ctx context.Context, client *clientv3.Client, compactInterval time.Duration) {
	endpointsMapMu.Lock()
	defer endpointsMapMu.Unlock()

   // 一个进程中，对一个集群只需要设定一个 compactor。apiserver 依赖 endpoint 列表区分不同的 etcd 集群。
	for _, ep := range client.Endpoints() {
	   // 如果 etcd 集群中任何一个 endpoint 有 compactor，则返回 
		if _, ok := endpointsMap[ep]; ok {
			return
		}
	}
	for _, ep := range client.Endpoints() {
		endpointsMap[ep] = struct{}{} // 存下来 endpoint，标记其已经有了 compactor
	}

	if compactInterval != 0 {
		go compactor(ctx, client, compactInterval)  // 异步启动 compactor
	}
}

// compactor 周期性的压缩 etcd 中的数据，把版本小于给定版本的数目全部删掉。
// 压缩后，任何获取小于指定版本的数据的请求都会被返回 error。
// @interval 是 compaction 周期时长。 apiserver 启动后第一次 compaction 时间点肯定晚于 @interval。
//
//  Technical definitions:
// k8s 在 etcd 中定义了一个特殊的 key 叫做 *compactRevKey*，value 是上次 compaction 版本号。
// compactRevKey 的值可以用作 compaction 的逻辑时钟，其初始值为 0。
//
// 算法：
// - 把本地的 compact_time 与 etcd 中的 compact_time 进行比较，判断二者是否相等。
// - 如果二者的值相等，则把两个值都增加 1，然后进行压缩。
// - 如果二者不相等，则把本地 compact_time 设定为 etcd 中的 compact_time 值。
//
//  Technical details/insights:
// 
//  这里面的协议细节是基于 lease。如果一个 apiserver  compactor CAS 执行成功，
//  则另一个 apiserver compactor 就会失败且会在 10 分钟后再次循环重试。
//  如果一个 APIServer 崩溃，另一个就可以 “接管” 过这个 compact 任务。
//  
//  例如，下图中，一个 compactor C1 在时间点 t1 和 t2 执行了 compaction，另一个 C2 在 t1' (t1 < t1' < t2)
//  执行 CAS 失败，则在 t1' 更新 oldRev，然后在 t2' (t2' > t2) 进行重试。如果 C1 崩溃，在 t2 没有执行 compact，
//  C2 会在 t2' 时刻结果 compact 任务。
// 
	//
	//             oldRev(t2)     curRev(t2)
	//                                 +
	//   oldRev    curRev       |
	//     +           +             |
	//     |             |              |
	//     |             |    t1'       |     t2'
	// +---v-------------v----^---------v------^---->
	//     t0           t1             t2
	//
//  
//  有如下保证：
// - 正常情况下，compaction 周期是 10 分钟。
// - 如果执行 compact 任务失败，则下次执行 compaction 时间是在 10m 之后，但不会晚于 20m【interval is >10m and <20m】。
//
// FAQ:
// - 如果 compaction 时间不精确怎么办？时间精确与否我们不 care，只有有人做 compaction 即可，compaction 任务通过 etcd API 保证其可以原子执行成功。
// - 任务 load 比较重时会发生什么？起始情况下一个 apiserver 每 10 分钟执行一次 compaction，它几乎不可能被程序 load 负载多寡影响。
func compactor(ctx context.Context, client *clientv3.Client, interval time.Duration) {
	var compactTime int64
	var rev int64
	var err error
	for {
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return
		}

		compactTime, rev, err = compact(ctx, client, compactTime, rev)
		if err != nil {
			klog.Errorf("etcd: endpoint (%v) compact failed: %v", client.Endpoints(), err)
			continue
		}
	}
}

// compact 压缩 etcd 存储并返回当前的数据版本。如果没有错误发生，返回的值是当前的 compact 时间和全局版本。注意，即使 CAS 失败也不会返回失败。
func compact(ctx context.Context, client *clientv3.Client, t, rev int64) (int64, int64, error) {
	resp, err := client.KV.Txn(ctx).If(
		clientv3.Compare(clientv3.Version(compactRevKey), "=", t),
	).Then(
		clientv3.OpPut(compactRevKey, strconv.FormatInt(rev, 10)), // Expect side effect: increment Version
	).Else(
		clientv3.OpGet(compactRevKey),
	).Commit()
	if err != nil {
		return t, rev, err
	}

	curRev := resp.Header.Revision

	if !resp.Succeeded {
		curTime := resp.Responses[0].GetResponseRange().Kvs[0].Version
		return curTime, curRev, nil
	}
	curTime := t + 1

	if rev == 0 {
		// We don't compact on bootstrap.
		return curTime, curRev, nil
	}
	if _, err = client.Compact(ctx, rev); err != nil {
		return curTime, curRev, err
	}
	klog.V(4).Infof("etcd: compacted rev (%d), endpoints (%v)", rev, client.Endpoints())
	return curTime, curRev, nil
}
```