

const (
	// We have set a buffer in order to reduce times of context switches.
	incomingBufSize = 100
	outgoingBufSize = 100
)

// fatalOnDecodeError is used during testing to panic the server if watcher encounters a decoding error
var fatalOnDecodeError = false

// errTestingDecode is the only error that testingDeferOnDecodeError catches during a panic
var errTestingDecode = errors.New("sentinel error only used during testing to indicate watch decoding error")

// errCreateObject is the only error that newFunc is nil in test cases
var errCreateObject = errors.New("failure to create obj as newFunc is nil")

// testingDeferOnDecodeError is used during testing to recover from a panic caused by errTestingDecode, all other values continue to panic
func testingDeferOnDecodeError() {
	if r := recover(); r != nil && r != errTestingDecode {
		panic(r)
	}
}

func init() {
	// check to see if we are running in a test environment
	TestOnlySetFatalOnDecodeError(true)
	fatalOnDecodeError, _ = strconv.ParseBool(os.Getenv("KUBE_PANIC_WATCH_DECODE_ERROR"))
}

// TestOnlySetFatalOnDecodeError should only be used for cases where decode errors are expected and need to be tested. e.g. conversion webhooks.
func TestOnlySetFatalOnDecodeError(b bool) {
	fatalOnDecodeError = b
}

type watcher struct {
	client      *clientv3.Client
	codec       runtime.Codec
	versioner   storage.Versioner
	transformer value.Transformer
	// newFunc is a function that creates new empty object storing a object.
	newFunc func() runtime.Object
	// watchBookmark feature-gate
	watchBookmarkEnabled bool
}

// watchChan implements watch.Interface.
type watchChan struct {
	watcher           *watcher
	key               string
	initialRev        int64
	recursive         bool
	internalPred      storage.SelectionPredicate
	ctx               context.Context
	cancel            context.CancelFunc
	incomingEventChan chan *event
	resultChan        chan watch.Event
	errChan           chan error
}

func newWatcher(client *clientv3.Client, codec runtime.Codec, versioner storage.Versioner, transformer value.Transformer, newFunc func() runtime.Object) *watcher {
	return &watcher{
		client:               client,
		codec:                codec,
		versioner:            versioner,
		transformer:          transformer,
		newFunc:              newFunc,
		watchBookmarkEnabled: utilfeature.DefaultFeatureGate.Enabled(features.WatchBookmark),
	}
}

// Watch 关注某个 key，返回一个 watch.Interface，用于传输相关的通知。
// 如果 @rev 是 0，则返回现存的 object(s)，然后从现存的 object 当前 (maximum revision+1)  开始关注。
// 如果 @rev 是 0，则关注 @rev 之后发生的版本的 event。
// 如果 @recursive 是 false，则只关注 @key。如果是 true，则会关注 @key 的下的所有的 children 和 目录，但不会关注 @key 自身。
// @pred 不能为空，因为函数只返回满足 @pred 的 event。
func (w *watcher) Watch(ctx context.Context, key string, rev int64, recursive bool, pred storage.SelectionPredicate) (watch.Interface, error) {
    // 确保获取其子节点
	if recursive && !strings.HasSuffix(key, "/") {
		key += "/"
	}
	wc := w.createWatchChan(ctx, key, rev, recursive, pred)
	go wc.run()
	return wc, nil
}

func (w *watcher) createWatchChan(ctx context.Context, key string, rev int64, recursive bool, pred storage.SelectionPredicate) *watchChan {
	wc := &watchChan{
		watcher:           w,
		key:               key,
		initialRev:        rev,
		recursive:         recursive,
		internalPred:      pred,
		incomingEventChan: make(chan *event, incomingBufSize),
		resultChan:        make(chan watch.Event, outgoingBufSize),
		errChan:           make(chan error, 1),
	}
	if pred.Empty() {
		// The filter doesn't filter out any object.
		wc.internalPred = storage.Everything
		wc.internalPred.AllowWatchBookmarks = pred.AllowWatchBookmarks
	}
	wc.ctx, wc.cancel = context.WithCancel(ctx)
	return wc
}

func (wc *watchChan) run() {
	watchClosedCh := make(chan struct{})
	go wc.startWatching(watchClosedCh)

	var resultChanWG sync.WaitGroup
	resultChanWG.Add(1)
	go wc.processEvent(&resultChanWG)

	select {
	case err := <-wc.errChan:
		if err == context.Canceled {
			break
		}
		errResult := transformErrorToEvent(err)
		if errResult != nil {
			// error result is guaranteed to be received by user before closing ResultChan.
			select {
			case wc.resultChan <- *errResult:
			case <-wc.ctx.Done(): // user has given up all results
			}
		}
	case <-watchClosedCh:
	case <-wc.ctx.Done(): // user cancel
	}

	// We use wc.ctx to reap all goroutines. Under whatever condition, we should stop them all.
	// It's fine to double cancel.
	wc.cancel()

	// we need to wait until resultChan wouldn't be used anymore
	resultChanWG.Wait()
	close(wc.resultChan)
}

func (wc *watchChan) Stop() {
	wc.cancel()
}

func (wc *watchChan) ResultChan() <-chan watch.Event {
	return wc.resultChan
}

// sync tries to retrieve existing data and send them to process.
// The revision to watch will be set to the revision in response.
// All events sent will have isCreated=true
func (wc *watchChan) sync() error {
	opts := []clientv3.OpOption{}
	if wc.recursive {
		opts = append(opts, clientv3.WithPrefix())
	}
	getResp, err := wc.watcher.client.Get(wc.ctx, wc.key, opts...)
	if err != nil {
		return err
	}
	wc.initialRev = getResp.Header.Revision
	for _, kv := range getResp.Kvs {
		wc.sendEvent(parseKV(kv))
	}
	return nil
}

// logWatchChannelErr checks whether the error is about mvcc revision compaction which is regarded as warning
func logWatchChannelErr(err error) {
	if !strings.Contains(err.Error(), "mvcc: required revision has been compacted") {
		klog.Errorf("watch chan error: %v", err)
	} else {
		klog.Warningf("watch chan error: %v", err)
	}
}

// startWatching does:
// - get current objects if initialRev=0; set initialRev to current rev
// - watch on given key and send events to process.
func (wc *watchChan) startWatching(watchClosedCh chan struct{}) {
	if wc.initialRev == 0 {
		if err := wc.sync(); err != nil {
			klog.Errorf("failed to sync with latest state: %v", err)
			wc.sendError(err)
			return
		}
	}
	opts := []clientv3.OpOption{
		clientv3.WithRev(wc.initialRev + 1),
		clientv3.WithPrevKV(),
		clientv3.WithProgressNotify(),
	}
	if wc.recursive {
		opts = append(opts, clientv3.WithPrefix())
	}
	wch := wc.watcher.client.Watch(wc.ctx, wc.key, opts...)
	for wres := range wch {
		if wres.Err() != nil {
			err := wres.Err()
			// If there is an error on server (e.g. compaction), the channel will return it before closed.
			logWatchChannelErr(err)
			wc.sendError(err)
			return
		}

		// Send progress notify event when bookmark is enabled.
		if wres.IsProgressNotify() && wc.watcher.watchBookmarkEnabled {
			wc.sendEvent(progressNotifyEvent(wres.Header.GetRevision()))
			continue
		}

		metrics.RecordEtcdWatcherChannelLength(wc.key, "incomingEventChan", len(wc.incomingEventChan))
		metrics.RecordEtcdWatcherChannelLength(wc.key, "resultChan", len(wc.resultChan))
		metrics.RecordEtcdWatcherChannelLength(wc.key, "errChan", len(wc.errChan))
		metrics.RecordEtcdWatcherEventCount(wc.key, len(wres.Events))

		now := time.Now()
		for _, e := range wres.Events {
			parsedEvent, err := parseEvent(e)
			if err != nil {
				logWatchChannelErr(err)
				wc.sendError(err)
				return
			}
			wc.sendEvent(parsedEvent)
		}
		metrics.RecordEtcdWatcherEventLatency(wc.key, time.Since(now))
	}
	// When we come to this point, it's only possible that client side ends the watch.
	// e.g. cancel the context, close the client.
	// If this watch chan is broken and context isn't cancelled, other goroutines will still hang.
	// We should notify the main thread that this goroutine has exited.
	close(watchClosedCh)
}

// processEvent processes events from etcd watcher and sends results to resultChan.
func (wc *watchChan) processEvent(wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		select {
		case e := <-wc.incomingEventChan:
			res := wc.transform(e)
			if res == nil {
				continue
			}
			if len(wc.resultChan) == outgoingBufSize {
				klog.V(3).Infof("Fast watcher, slow processing. Number of buffered events: %d."+
					"Probably caused by slow dispatching events to watchers", outgoingBufSize)
			}
			// If user couldn't receive results fast enough, we also block incoming events from watcher.
			// Because storing events in local will cause more memory usage.
			// The worst case would be closing the fast watcher.
			select {
			case wc.resultChan <- *res:
			case <-wc.ctx.Done():
				return
			}
		case <-wc.ctx.Done():
			return
		}
	}
}

func (wc *watchChan) filter(obj runtime.Object) bool {
	if wc.internalPred.Empty() {
		return true
	}
	matched, err := wc.internalPred.Matches(obj)
	return err == nil && matched
}

func (wc *watchChan) acceptAll() bool {
	return wc.internalPred.Empty()
}

// transform transforms an event into a result for user if not filtered.
func (wc *watchChan) transform(e *event) (res *watch.Event) {
	curObj, oldObj, err := wc.prepareObjs(e)
	if err != nil {
		klog.Errorf("failed to prepare current and previous objects: %v", err)
		wc.sendError(err)
		return nil
	}

	switch {
	case e.isDeleted:
		if !wc.filter(oldObj) {
			return nil
		}
		res = &watch.Event{
			Type:   watch.Deleted,
			Object: oldObj,
		}
	case e.isCreated:
		if !wc.filter(curObj) {
			return nil
		}
		res = &watch.Event{
			Type:   watch.Added,
			Object: curObj,
		}
	case e.isProgressNotify:
		if !wc.watcher.watchBookmarkEnabled || !wc.internalPred.AllowWatchBookmarks {
			return nil
		}
		res = &watch.Event{
			Type:   watch.Bookmark,
			Object: curObj,
		}
	default:
		if wc.acceptAll() {
			res = &watch.Event{
				Type:   watch.Modified,
				Object: curObj,
			}
			return res
		}
		curObjPasses := wc.filter(curObj)
		oldObjPasses := wc.filter(oldObj)
		switch {
		case curObjPasses && oldObjPasses:
			res = &watch.Event{
				Type:   watch.Modified,
				Object: curObj,
			}
		case curObjPasses && !oldObjPasses:
			res = &watch.Event{
				Type:   watch.Added,
				Object: curObj,
			}
		case !curObjPasses && oldObjPasses:
			res = &watch.Event{
				Type:   watch.Deleted,
				Object: oldObj,
			}
		}
	}
	return res
}

func transformErrorToEvent(err error) *watch.Event {
	err = interpretWatchError(err)
	if _, ok := err.(apierrors.APIStatus); !ok {
		err = apierrors.NewInternalError(err)
	}
	status := err.(apierrors.APIStatus).Status()
	return &watch.Event{
		Type:   watch.Error,
		Object: &status,
	}
}

func (wc *watchChan) sendError(err error) {
	select {
	case wc.errChan <- err:
	case <-wc.ctx.Done():
	}
}

func (wc *watchChan) sendEvent(e *event) {
	if len(wc.incomingEventChan) == incomingBufSize {
		klog.V(3).Infof("Fast watcher, slow processing. Number of buffered events: %d."+
			"Probably caused by slow decoding, user not receiving fast, or other processing logic",
			incomingBufSize)
	}
	select {
	case wc.incomingEventChan <- e:
	case <-wc.ctx.Done():
	}
}

func (wc *watchChan) prepareObjs(e *event) (curObj runtime.Object, oldObj runtime.Object, err error) {
	if e.isProgressNotify {
		if wc.watcher.newFunc == nil {
			return nil, nil, errCreateObject
		}
		obj := wc.watcher.newFunc()
		if err := wc.watcher.versioner.UpdateObject(obj, uint64(e.rev)); err != nil {
			return nil, nil, fmt.Errorf("failure to version api object (%d) %#v: %v", e.rev, obj, err)
		}
		return obj, nil, nil
	}
	if !e.isDeleted {
		data, _, err := wc.watcher.transformer.TransformFromStorage(e.value, authenticatedDataString(e.key))
		if err != nil {
			return nil, nil, err
		}
		curObj, err = decodeObj(wc.watcher.codec, wc.watcher.versioner, data, e.rev)
		if err != nil {
			return nil, nil, err
		}
	}
	// We need to decode prevValue, only if this is deletion event or
	// the underlying filter doesn't accept all objects (otherwise we
	// know that the filter for previous object will return true and
	// we need the object only to compute whether it was filtered out
	// before).
	if len(e.prevValue) > 0 && (e.isDeleted || !wc.acceptAll()) {
		data, _, err := wc.watcher.transformer.TransformFromStorage(e.prevValue, authenticatedDataString(e.key))
		if err != nil {
			return nil, nil, err
		}
		// Note that this sends the *old* object with the etcd revision for the time at
		// which it gets deleted.
		oldObj, err = decodeObj(wc.watcher.codec, wc.watcher.versioner, data, e.rev)
		if err != nil {
			return nil, nil, err
		}
	}
	return curObj, oldObj, nil
}

func decodeObj(codec runtime.Codec, versioner storage.Versioner, data []byte, rev int64) (_ runtime.Object, err error) {
	obj, err := runtime.Decode(codec, []byte(data))
	if err != nil {
		if fatalOnDecodeError {
			// catch watch decode error iff we caused it on
			// purpose during a unit test
			defer testingDeferOnDecodeError()
			// we are running in a test environment and thus an
			// error here is due to a coder mistake if the defer
			// does not catch it
			panic(err)
		}
		return nil, err
	}
	// ensure resource version is set on the object we load from etcd
	if err := versioner.UpdateObject(obj, uint64(rev)); err != nil {
		return nil, fmt.Errorf("failure to version api object (%d) %#v: %v", rev, obj, err)
	}
	return obj, nil
}
