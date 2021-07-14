// Copyright 2015 The etcd Authors
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

// Package backend defines a standard interface for etcd's backend MVCC storage.
package backend

// backend 整体执行的是读写分离的逻辑，有关于读的接口都在 read_tx.go readTx & concurrentReadTx。
// 有关于写的接口则是在 batch_tx.go batchTxBuffered 中。
