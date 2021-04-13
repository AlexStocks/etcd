# etcd

## etcd dir

```

  ├── Documentation  # 文档
  ├── auth  # 认证
  ├── bin  # 编译出来的二进制文件
  ├── client  # 应该是v2版本的客户端代码
  ├── clientv3  # 应该是v3版本的客户端代码
  ├── contrib  # 今天我们要看的raftexample就在这里面
  ├── default.etcd  # 运行编译好的etcd产生的，忽略之
  ├── docs  # 文档
  ├── embed  # 封装了etcd的函数，以便别的程序封装
  ├── etcdctl  # etcdctl命令，也就是客户端
  ├── etcdmain  # main.go 调用了这里
  ├── etcdserver  # 服务端代码
  ├── functional  # 不知道是干啥的，看起来是用来验证功能的测试套件
  ├── hack  # 开发者用的
  ├── integration
  ├── lease  # 实现etcd的租约
  ├── logos
  ├── mvcc # MVCC存储的实现
  ├── pkg  # 通用库
  ├── proxy  # 代理
  ├── raft  # raft一致性协议的实现
  ├── scripts  # 各种脚本
  ├── tests
  ├── tools  # 一些工具
  ├── vendor  # go的vendor，忽略
  ├── version  # 版本信息
  └── wal  # Write-Ahead-Log的实现

```
