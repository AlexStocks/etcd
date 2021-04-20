# WAL

dir

```text
.
├── decoder.go
├── doc.go
├── encoder.go
├── file_pipeline.go
├── file_pipeline_test.go
├── metrics.go
├── record_test.go
├── repair.go
├── repair_test.go
├── util.go
├── wal.go
├── wal_bench_test.go
├── wal_test.go
└── walpb
    ├── record.go
    ├── record.pb.go
    └── record.proto
```

Package wal provides an implementation of a write ahead log that is used by
etcd.

一个 WAL 是一个特定的数据目录，其中由个 WAL 文件，每个文件都是一个 wal segment。每个文件内部都存储了 raft state 和 log entry 集合。 

```go
	metadata := []byte{}
	w, err := wal.Create(zap.NewExample(), "/var/lib/etcd", metadata)
	...
	err := w.Save(s, ents)
```

然后再存储 snapshot。

```go
	err := w.SaveSnapshot(walpb.Snapshot{Index: 10, Term: 2})
```

写完，把文件关闭掉。

```go
	w.Close()
```

WAL 文件是一段 WAL record 集合。

Each WAL file is a stream of WAL records. A WAL record is a length field and a wal record
protobuf. The record protobuf contains a CRC, a type, and a data payload. The length field is a
64-bit packed structure holding the length of the remaining logical record data in its lower
56 bits and its physical padding in the first three bits of the most significant byte. Each
record is 8-byte aligned so that the length field is never torn. The CRC contains the CRC32
value of all record protobufs preceding the current record.

WAL files are placed inside of the directory in the following format:
$seq-$index.wal

The first WAL file to be created will be 0000000000000000-0000000000000000.wal
indicating an initial sequence of 0 and an initial raft index of 0. The first
entry written to WAL MUST have raft index 0.

WAL will cut its current tail wal file if its size exceeds 64MB. This will increment an internal
sequence number and cause a new file to be created. If the last raft index saved
was 0x20 and this is the first time cut has been called on this WAL then the sequence will
increment from 0x0 to 0x1. The new file will be: 0000000000000001-0000000000000021.wal.
If a second cut issues 0x10 entries with incremental index later then the file will be called:
0000000000000002-0000000000000031.wal.

At a later time a WAL can be opened at a particular snapshot. If there is no
snapshot, an empty snapshot should be passed in.

	w, err := wal.Open("/var/lib/etcd", walpb.Snapshot{Index: 10, Term: 2})
	...

The snapshot must have been written to the WAL.

Additional items cannot be Saved to this WAL until all of the items from the given
snapshot to the end of the WAL are read first:

	metadata, state, ents, err := w.ReadAll()

This will give you the metadata, the last raft.State and the slice of
raft.Entry items in the log.

我们先阅读 doc.go，可以知道这些东西：

  * WAL这个抽象的结构体是由一堆的文件组成的
  * 每个WAL文件的头部有一部分数据，是metadata
  * 使用 w.Save 保存数据
  * 使用完成之后，使用 w.Close 关闭
  * WAL中的每一条记录，都有一个循环冗余校验码（CRC）
  * WAL是只能打开来用于读，或者写，但是不能既读又写
  
  
 预写式日志(Write Ahead Log, WAL)，其核心思想是，所有的修改在提交之前都要先写入 log，通过日志记录描述好数据的改变后，再写入缓存，等缓存区写满后，再往持久层修改数据。目的在于，在系统崩溃后，能够在日志指导下恢复到崩溃前的状态，避免数据丢失。
 
 WAL 详细介绍：
 
 主要文件
 代码位置：etcd/wal
 
 1.wal.go
 wal 日志的具体实现文件，包括写入日志、切换日志文件、读取 wal 日志、释放文件等操作。wal 文件名命名方式为 seq-index.log，seq 为递增序列号，在每次切换日志是时 seq+1。index 是第一条日志的索引值(物理偏移量)。
 
 2.file_pipline.go
 实现文件的预生成，在 wal 运行过程中，run() 函数会生成一个 x.tmp 文件并且预分配好空间(64M)，等待 wal log 被写满之后(64M)后，会用这个 x.tmp 作为新的 wal log。
 tmp 文件的命名方式为 (count%2).tmp, count 是生成文件的次数。即文件名只可能是 0.tmp 或 1.tmp
 
 3.encoder.go
 实现日志的记录序列化、字节对齐与填充(8 Byte 对齐)、缓存日志、刷盘等操作。
 
 4.etcd/pkg/ioutil/pagewriter.go
 encoder.go 中，通过 pagewriter 实现缓存，pagewriter 实现 128K 的缓冲，并在缓冲区满之后，写入磁盘。同时保证 NK(4K) 的字节对齐。 
 
 5.decoder.go
 解析 wal log 的记录，并反序列化。
 
 wal log 创建流程
 文件：wal.go
 函数：Create()
 流程：
 (1)创建临时wal 目录
 (2)生成第一个 wal 日志 0-0.wal, 名字生成方法参考 walName() 函数
 (3)wal 日志文件加锁，防止其他程序写入
 (4)预分配空间 64M，以保证 WAL 有足够的磁盘空间，避免在写入过程中，因磁盘空间不足，导致写入失败。
 (5)定义 encoder，并将 io.writer 指向 wal 日志
 (6)追加文件句柄到 locks 中， 这是为了记录被加锁的文件，以便日后释放，同时 locks 中最后一个文件句柄是当前正在操作的文件句柄。
 (7)写入各种日志其中包括
     crc 校验码
     metadata 元数据
     Snapshot 空快照记录
 (8)将临时 wal 目录重命名为正式的 wal 目录， 并且启动 file_pipeline。 file_pipeline 主要作用是预生成一个 x.tmp 文件，并预分配内存，等待 wal 日志写满后使用。
 (9)刷盘进行持久化。
 
 日志写入
 文件：wal.go
 函数：Save()
 流程：
 (1)saveEntry  写入 entry 记录
 (2)saveState  写入 hardState 记录
 (3)判断是否写满(SegmentSizeBytes), 如果文件写满，切换日志文件。
 
 切换日志文件
 文件：wal.go
 函数：cut()
 流程：
 (1)执行刷盘操作 sync
 (2)获取一个新的 wal 文件名
 (3)从 file_pipeline 中获取以准备好的文件句柄
 (4)locks 中添加新的文件句柄
 (5)encoder 执行新的文件句柄
 (6)新文件写入各种日志其中包括
     crc 校验码
     metadata 元数据
     hardState  节点信息
     Snapshot 空快照记录
 (7)将上述信息刷入磁盘 sync
 (8)将从 file_pipeline 中的文件句柄，重新命名为(2)中获取的文件名。
 (9)关闭临时文件，重新打开重命名后的文件。执行(3)(4)操作，再预生成一个文件。
 
 释放 locks 中的文件
 文件：wal.go
 函数：ReleaseLockTo()
 
 for i, l := range w.locks {
 		//将文件名解析成 seq 和 index
 _, lockIndex, err := parseWALName(filepath.Base(l.Name()))
 		if err != nil {
 			return err
 		}
 		if lockIndex >= index { 
 			smaller = i - 1
 			found = true
 			break
 		}
 	}
 ...
 	for i := 0; i < smaller; i++ { 
 		if w.locks[i] == nil {
 			continue
 		}
 		w.locks[i].Close()
 	}
 	w.locks = w.locks[smaller:]
 }
 系统加载已存在 wal
 文件：wal.go
 函数：openAtIndex()
 (1)找到所有 wal 文件名 names，且根据 wal 命名规则，找到 Index 最大且小于 snap.Index 的 wal 日志在 names 中的索引位置 nameIndex ，函数 selectWALFiles 。
 (2)根据(1)的结果，读取 wal 日志， 函数 openWALFiles。
 (3)创建 wal 实例。
 
 日志回放
 文件：wal.go
 函数：ReadAll()
 流程：
 (1)在创建 wal 实例过程， 会加载所有符合条件的 wal 文件句柄到 decoder 中(参考 openAtIndex)。 日志回放可以根据 decoder 中文件句柄，进行数据读取和回放。
 (2)decode 解析数据
 
 func (d *decoder) decodeRecord(rec *walpb.Record) error {
     if len(d.brs) == 0 { // brs表示一个可读取的文件句柄数组，等于0表示没有可读的文件了
         return io.EOF
     }
     l, err := readInt64(d.brs[0]) // 读取日志的长度
     //是否读到了尾部
     if err == io.EOF || (err == nil && l == 0) {
         d.brs = d.brs[1:] //读取下一个文件句柄数据
         if len(d.brs) == 0 { // 后面没有日志可读取了
             return io.EOF
         }
         d.lastValidOff = 0   
 return d.decodeRecord(rec) 
     }
     if err != nil {
         return err
     }
 /*
 计算日志真实长度和填充的长度
 读取数据所有数据（真实长度+填充长度）
 根据真实长度，反序列化日志记录。
 ....
 */
 }
 (3)根据消息类型，更新对应数据。
 
    //读取decorder 中的所有日志
 for err = decoder.decode(rec); err == nil; err = decoder.decode(rec) {
 switch rec.Type { //根据日志类型分类
         case entryType:
 ...
         case stateType: //
             ...
         case metadataType:
 ...
         case crcType:
 ...
         case snapshotType:
 ...
         default:
 ...
         }
  
 }
 总结
 1. wal 文件的命名的规则  seq_index。 这个可以保证重启的时候，根据系统上一次持久化的最大 Index，找到所有大于 Index 的 wal 进行回放，同时所有小于 Index 的 wal 文件都会定时删除。
 
 2. wal  会通过 file_pipeline 来持续预生成和预分配空间一个临时文件，以保证 wal 文件写满之后，可以马上 获取下一个可使用的新 wal 文件。
 
 3. wal 通过 pagewriter 实现 128K 缓冲，以减少刷盘次数，提高写的效率。
 
 
 ————————————————
 版权声明：本文为CSDN博主「huang_0_3」的原创文章，遵循CC 4.0 BY-SA版权协议，转载请附上原文出处链接及本声明。
 原文链接：https://blog.csdn.net/H_L_S/article/details/112380858 