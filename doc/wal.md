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
  
  
  