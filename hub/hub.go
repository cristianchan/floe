package hub

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/floeit/floe/client"
	"github.com/floeit/floe/config"
	nt "github.com/floeit/floe/config/nodetype"
	"github.com/floeit/floe/event"
	"github.com/floeit/floe/log"
	"github.com/floeit/floe/store"
)

// some special system event tags, generated by the system internals rather than the configurable nodes
const (
	tagEndFlow     = "sys.end.all"       // a run has ended
	tagNodeUpdate  = "sys.node.update"   // an executing node has had an update to its output
	tagNodeStart   = "sys.node.start"    // an executing node has started its job
	tagStateChange = "sys.state"         // a run has transitioned state
	tagWaitingData = "sys.data.required" // a node in the run needs data input
	tagGoodTrigger = "trigger.good"      // always issued when a trigger

	inboundPrefix = "inbound" // the tags from any data push events
)

var zt = time.Time{} // zero time

// node definitions
type refNode interface {
	NodeRef() config.NodeRef
	GetTag(string) string
}

type exeNode interface {
	refNode
	Execute(*nt.Workspace, nt.Opts, chan string) (int, nt.Opts, error)
	Status(status int) (string, bool)
}

type mergeNode interface {
	refNode
	TypeOfNode() string
	Waits() int
}

// Hub links events to the config rules
type Hub struct {
	sync.RWMutex

	basePath string         // the configured basePath for the hub
	hostID   string         // the id fo this host
	config   *config.Config // the config rules
	queue    *event.Queue   // the event q to route all events

	// tags
	tags []string // the tags that

	// hosts lists all the hosts
	hosts []*client.FloeHost

	// runs contains list of runs ongoing or the archive
	// this is the only ongoing changing state the hub manages
	// the runstore is responsible for persisting any state
	runs *RunStore
}

// New creates a new hub with the given config
func New(host, tags, basePath, adminTok string, c *config.Config, s store.Store, q *event.Queue) *Hub {
	// create all tags
	l := strings.Split(tags, ",")
	tagList := []string{}
	for _, t := range l {
		tagList = append(tagList, strings.TrimSpace(t))
	}
	h := &Hub{
		hostID:   host,
		tags:     tagList,
		basePath: basePath,
		config:   c,
		queue:    q,
		runs:     newRunStore(s),
	}
	// setup hosts
	h.setupHosts(adminTok)
	// hub subscribes to its own queue
	h.queue.Register(h)
	// start checking the pending queue
	go h.serviceLists()

	return h
}

// HostID returns the id for this host
func (h *Hub) HostID() string {
	return h.hostID
}

// Tags returns the server tags
func (h *Hub) Tags() []string {
	return h.tags
}

// AllClientRuns queries all hosts for their summaries for the given run ID
func (h *Hub) AllClientRuns(flowID string) client.RunSummaries {
	s := client.RunSummaries{}
	for _, host := range h.hosts {
		summaries := host.GetRuns(flowID)
		s.Append(summaries)
	}
	return s
}

// AllClientFindRun queries all hosts for the specified run
func (h *Hub) AllClientFindRun(flowID, runID string) *client.Run {
	for _, host := range h.hosts {
		run := host.FindRun(flowID, runID)
		if run != nil {
			return run
		}
	}
	return nil
}

// AllHosts returns all the hosts
func (h *Hub) AllHosts() map[string]client.HostConfig {
	h.Lock()
	defer h.Unlock()
	r := map[string]client.HostConfig{}
	for _, host := range h.hosts {
		c := host.GetConfig()
		r[c.HostID] = c
	}
	return r
}

// Config returns the config for this hub
func (h *Hub) Config() config.Config {
	return *h.config
}

// AllRuns returns all the runs for this hub.
func (h *Hub) AllRuns(id string) (pending Runs, active Runs, archive Runs) {
	return h.runs.allRuns(id)
}

// FindRun returns an individual run as given by the flow and run.
func (h *Hub) FindRun(flowID, runID string) *Run {
	return h.runs.find(flowID, runID)
}

// Queue returns the hubs queue
func (h *Hub) Queue() *event.Queue {
	return h.queue
}

// Notify is called whenever an event is sent to the hub. It
// makes the hub an event.Observer
func (h *Hub) Notify(e event.Event) {
	// if the event has not been previously adopted in any pending run then it is a trigger event
	if !e.RunRef.Adopted() {
		err := h.pendFlowFromTrigger(e)
		if err != nil {
			log.Error(err)
		}
		return
	}
	// otherwise it is a run specific event
	h.dispatchToActive(e)
}

func (h *Hub) activate(pend *Pend, hostID string) error {
	err := h.runs.activate(pend, h.hostID)
	if err != nil {
		return err
	}
	h.queue.Publish(event.Event{
		RunRef: pend.Ref,
		Tag:    tagStateChange,
		Opts: nt.Opts{
			"action": "activate",
		},
		Good: true,
	})
	return nil
}

// ExecutePending executes a pending on this host - if this host has no conflicts.
// This could have been called directly if this is the only host, or could have
// been called via the server API as this host has been asked to accept the run.
// The boolean returned represents whether the flow was considered dealt with,
// meaning an attempt to start executing it occurred.
func (h *Hub) ExecutePending(pend Pend) (bool, error) {
	log.Debugf("<%s> - exec - attempt to execute pending type:<%s>", pend, pend.InitiatingEvent.Tag)

	flow, ok := h.config.FindFlow(pend.Ref.FlowRef, pend.InitiatingEvent.Tag, pend.InitiatingEvent.Opts)
	if !ok {
		return false, fmt.Errorf("pending flow not known %s, %s", pend.Ref.FlowRef, pend.InitiatingEvent.Tag)
	}

	// confirm no currently executing flows have a resource flag conflicts
	active := h.runs.activeFlows()
	log.Debugf("<%s> - exec - checking active conflicts with %d active runs", pend, len(active))
	for _, aRef := range active {
		fl := h.config.Flow(aRef)
		if fl == nil {
			log.Error("Strange that we have an active flow without a matching config", aRef)
			continue
		}
		if anyTags(fl.ResourceTags, flow.ResourceTags) {
			log.Debugf("<%s> - exec - found resource tag conflict on tags: %v with already active tags: %v",
				pend, flow.ResourceTags, fl.ResourceTags)
			return false, nil
		}
	}

	// setup the workspace config
	_, err := h.enforceWS(pend.Ref, flow.ReuseSpace)
	if err != nil {
		return false, err
	}

	// add the active flow
	err = h.activate(&pend, h.hostID)
	if err != nil {
		return false, err
	}

	log.Debugf("<%s> - exec - triggering %d nodes", pend, len(flow.Nodes))

	// and then emit the trigger event that were tripped when this flow was made pending
	// (more than one trigger at a time is going to be pretty rare)
	for _, n := range flow.Nodes {
		h.queue.Publish(event.Event{
			RunRef:     pend.Ref,
			SourceNode: n.NodeRef(),
			Tag:        tagGoodTrigger,            // all triggers emit the same event
			Opts:       pend.InitiatingEvent.Opts, // make sure we have the trigger event data
			Good:       true,                      // all trigger events that start a run must be good
		})
	}

	return true, nil
}

// serviceLists attempts to dispatch pending flows and times outs
// any active flows that are past their deadline
func (h *Hub) serviceLists() {
	for range time.Tick(time.Second) {
		err := h.distributeAllPending()
		if err != nil {
			log.Error(err)
		}
	}
}

// removePend removes the pend from the pending list.
// Any error returned will be in the persisting of the pending list.
func (h *Hub) removePend(pend Pend) error {
	ok, err := h.runs.removePend(pend)
	if err != nil {
		return err
	}
	// if this did remove it from the pending list then send the system event
	if ok {
		h.queue.Publish(event.Event{
			RunRef: pend.Ref,
			Tag:    tagStateChange,
			Opts: nt.Opts{
				"action": "remove-pend",
			},
			Good: true,
		})
	}
	return nil
}

// distributeAllPending loops through all pending runs assessing whether they can be run then distributes them.
func (h *Hub) distributeAllPending() error {
	for _, p := range h.runs.allPends() {
		log.Debugf("<%s> - pending - attempt dispatch", p)

		if len(h.hosts) == 0 {
			log.Debugf("<%s> - pending - no hosts configured running job locally", p)
			ok, err := h.ExecutePending(p)
			if err != nil {
				return err
			}
			if !ok {
				log.Debugf("<%s> - pending - could not run job locally yet", p)
			} else {
				log.Debugf("<%s> - pending - job started locally", p)
				if err := h.removePend(p); err != nil {
					log.Error("could not save pending removal", err)
				}
			}
			continue
		}

		// as we have some hosts configured - attempt to schedule them
		// first find the matching flow
		flow, ok := h.config.FindFlow(p.Ref.FlowRef, p.InitiatingEvent.Tag, p.InitiatingEvent.Opts)
		if !ok {
			if err := h.removePend(p); err != nil {
				return err
			}
			// TODO update status of the run - to indicate error
			// TODO possibly issue a system event to inform the UI of this failure
			return fmt.Errorf("pending not found %s, %s removed from pending", p, p.InitiatingEvent.Tag)
		}

		log.Debugf("<%s> - pending - found flow %s tags: %v", p, flow.Ref, flow.HostTags)

		// Find candidate hosts that have a superset of the tags for the pending flow
		candidates := []*client.FloeHost{}
		for _, host := range h.hosts {
			cfg := host.GetConfig()
			log.Debugf("<%s> - pending - testing host %s with host tags: %v", p, cfg.HostID, cfg.Tags)
			if cfg.TagsMatch(flow.HostTags) {
				log.Debugf("<%s> - pending - found matching host %s with host tags: %v", p, cfg.HostID, cfg.Tags)
				candidates = append(candidates, host)
			}
		}

		log.Debugf("<%s> - pending - found %d candidate hosts", p, len(candidates))

		// attempt to send it to any of the candidates
		launched := false
		for _, host := range candidates {
			if host.AttemptExecute(p.Ref, p.InitiatingEvent) {
				log.Debugf("<%s> - pending - executed on <%s>", p, host.GetConfig().HostID)
				// remove from our pending list
				if err := h.removePend(p); err != nil {
					log.Error("could not save pending removal", err)
				}
				launched = true
				break
			}
		}

		if !launched {
			log.Debugf("<%s> - pending - no available host yet", p)
		}

		// TODO check pending queue for any pending run that is over age and send alert
	}
	return nil
}

func (h *Hub) addToPending(flow config.FlowRef, hostID string, e event.Event) (event.RunRef, error) {
	ref, err := h.runs.addToPending(flow, hostID, e)
	if err != nil {
		return ref, err
	}

	h.queue.Publish(event.Event{
		RunRef: ref,
		Tag:    tagStateChange,
		Opts: nt.Opts{
			"action": "add-pend",
		},
		Good: true,
	})

	return ref, nil
}

// pendFlowFromTrigger uses the subscription fired event e to put a FoundFlow
// on the pending queue, storing the initial event for use as the run is executed.
func (h *Hub) pendFlowFromTrigger(e event.Event) error {
	if !strings.HasPrefix(e.Tag, inboundPrefix) {
		return fmt.Errorf("event %s dispatched to triggers does not have inbound tag prefix", e.Tag)
	}
	triggerType := e.Tag[len(inboundPrefix)+1:]

	log.Debugf("attempt to trigger type:<%s> (specified flow: %v)", triggerType, e.RunRef.FlowRef)

	// find any Flows with subs matching this event
	found := h.config.FindFlowsByTriggers(triggerType, e.RunRef.FlowRef, e.Opts)
	if len(found) == 0 {
		log.Debugf("no matching flow for type:'%s' (specified flow: %v)", triggerType, e.RunRef.FlowRef)
		return nil
	}

	// the event tag should now match the trigger type
	e.Tag = triggerType

	// add each flow to the pending list
	for f, ff := range found {
		// copy the matched node to the source node for the trigger that this event matched
		e.SourceNode = ff.Nodes[0].Ref
		ref, err := h.addToPending(f, h.hostID, e)
		if err != nil {
			return err
		}
		log.Debugf("<%s> - from trigger type '%s' added to pending", ref, triggerType)
	}
	return nil
}

// dispatchToActive takes event e and routes it to the specific active flow as detailed in e
func (h *Hub) dispatchToActive(e event.Event) {
	// We dont care about these system events
	if e.IsSystem() {
		return
	}

	// for all active flows find ones that match
	_, r := h.runs.findActiveRun(e.RunRef.Run)
	if r == nil {
		// no matching active run - throw the events away
		log.Debugf("<%s> - dispatch - event '%s' received, but run not active (ignoring event)", e.RunRef, e.Tag)
		return
	}

	// is it an inbound data requests
	if strings.HasPrefix(e.Tag, inboundPrefix) {
		// inbound data events sent to an active run must be targetting a specific node.
		// in which case the event SourceNode is the source that requested the data input, so
		// is therefore also the target in this case.
		// the flow is specified, as is the target node
		flow := h.config.Flow(e.RunRef.FlowRef)
		if flow == nil {
			log.Errorf("<%s> - dispatch - no flow for inbound data event flow: %s", e.RunRef, e.RunRef.FlowRef)
			return
		}
		// strip off the inbound prefix
		e.Tag = e.Tag[len(inboundPrefix)+1:]
		node := flow.Node(e.SourceNode.ID)
		if node == nil {
			log.Errorf("<%s> - dispatch - no node in flow for inbound data event flow: %s, node: %s", e.RunRef, e.RunRef.FlowRef, e.SourceNode.ID)
			return
		}

		// tell the node we have some more data
		h.setFormData(r, node, e.Opts)
		return
	}

	// find all specific nodes from the config that listen for this event
	found, flowExists := h.config.FindNodeInFlow(r.Ref.FlowRef, e.Tag)
	if !flowExists {
		log.Errorf("<%s> - dispatch - no flow for event '%s'", e.RunRef, e.Tag)
		// this is indeed a strange occurrence so this run is considered both bad and incomplete
		h.endRun(r, e.SourceNode, e.Opts, "incomplete", false)
		return
	}

	// We got a matching flow but no nodes matched this event in the flow.
	// TODO I think we should be able to decide if dangling nodes (events that are not routed)
	// end the flow - for now - they do - so be sure to route all events
	if len(found.Nodes) == 0 {
		if e.Good {
			// The run ended with a good node, but that was not explicitly routed so the run is considered incomplete.
			// TODO allow dangling event - so the other events can have a chance to finish.
			// All good statuses should make it to a next node, so log the warning that this one has not.
			log.Errorf("<%s> - dispatch - nothing listening to good event '%s' - prematurely end", e.RunRef, e.Tag)
			h.endRun(r, e.SourceNode, e.Opts, "incomplete", true)
		} else {
			// bad events un routed can implicitly trigger the end of a run,
			// with the run marked bad
			log.Debugf("<%s> - dispatch - nothing listening to bad event '%s' (ending flow as bad)", e.RunRef, e.Tag)
			h.endRun(r, e.SourceNode, e.Opts, "complete", false)
		}
		return
	}

	// Fire all matching nodes
	for _, n := range found.Nodes {
		switch n.Class {
		case config.NcTask:
			switch nt.NType(n.TypeOfNode()) {
			case nt.NtEnd: // special task type end the run
				h.endRun(r, n.NodeRef(), e.Opts, "complete", e.Good)
				return
			case nt.NtData: // initial event triggering a data node
				h.setFormData(r, n, e.Opts)
			default:
				// asynchronous execute
				go h.executeNode(r, n, e, found.ReuseSpace)
			}
		case config.NcMerge:
			h.mergeEvent(r, n, e)
		}
	}
}

// setFormData - sets the opts form data on the active run. If the form is incomplete it
// emits a system event which no other node should will be listening for so will effectively
// pause the run, until later when any inbound data triggers the event for this data node.
// ultimately either explicitly marking the node good or bad, and issuing the appropriate event.
func (h *Hub) setFormData(run *Run, node exeNode, opts nt.Opts) {

	// keep the filled in form values separate from the config opts
	valOpts := nt.Opts{}
	valOpts["values"] = opts

	// status 0 = good, 1 = bad, 2 = needs more data,
	status, outOpts, _ := node.Execute(nil, valOpts, nil)

	// add the form fields to the flow
	h.runs.updateDataNode(run, node.NodeRef().ID, outOpts)

	ev := event.Event{
		RunRef:     run.Ref,
		SourceNode: node.NodeRef(),
		Opts:       outOpts,
	}
	switch status {
	case 0: // form data accepted and marking the node good
		ev.Tag = node.GetTag("good")
		ev.Good = true
		h.queue.Publish(ev)
	case 1:
		// form data accepted but mark the node bad
		ev.Tag = node.GetTag("bad")
		ev.Good = false
		h.queue.Publish(ev)
	case 2:
		// more data input is needed
		ev.Tag = tagWaitingData
		ev.Good = true
		h.queue.Publish(ev)
	}
}

// publishIfActive publishes the event if the run is still active
func (h *Hub) publishIfActive(e event.Event) {
	_, r := h.runs.findActiveRun(e.RunRef.Run)
	if r == nil {
		return
	}
	h.queue.Publish(e)
}

// executeNode invokes a task node Execute function for the active run
func (h *Hub) executeNode(run *Run, node exeNode, e event.Event, singleWs bool) {
	runRef := run.Ref
	nodeID := node.NodeRef().ID
	log.Debugf("<%s> - exec node - event tag: %s, node: %s", runRef, e.Tag, nodeID)

	// setup the workspace config
	ws, err := h.getWorkspace(runRef, singleWs)
	if err != nil {
		log.Debugf("<%s> - exec node - error getting workspace %v", runRef, err)
		return
	}

	// capture and emit all the node updates
	updates := make(chan string)
	go func() {
		for update := range updates {
			h.queue.Publish(event.Event{
				RunRef:     runRef,
				SourceNode: node.NodeRef(),
				Tag:        tagNodeUpdate,
				Opts: nt.Opts{
					"update": update,
				},
				Good: true,
			})

			// explicitly update any exec nodes with the ongoing execute
			h.runs.updateExecNode(run, nodeID, zt, zt, false, update)
		}
	}()

	// send the node start event
	h.publishIfActive(event.Event{
		RunRef:     runRef,
		SourceNode: node.NodeRef(),
		Tag:        tagNodeStart,
	})

	// set the start time for the node
	h.runs.updateExecNode(run, nodeID, time.Now(), zt, false, "")

	status, outOpts, err := node.Execute(ws, e.Opts, updates)
	close(updates)

	if err != nil {
		log.Errorf("<%s> - exec node (%s) - execute produced error: %v", runRef, node.NodeRef(), err)
		// publish the fact an internal node error happened
		h.publishIfActive(event.Event{
			RunRef:     runRef,
			SourceNode: node.NodeRef(),
			Tag:        node.GetTag("error"),
			Opts:       outOpts,
			Good:       false,
		})
		h.runs.updateExecNode(run, nodeID, zt, time.Now(), false, "")
		return
	}

	// construct event based on the Execute exit status
	ne := event.Event{
		RunRef:     runRef,
		SourceNode: node.NodeRef(),
		Opts:       outOpts,
	}

	// construct the event tag
	tagbit, good := node.Status(status)
	ne.Tag = node.GetTag(tagbit)
	ne.Good = good

	h.runs.updateExecNode(run, nodeID, zt, time.Now(), good, "")

	// and publish it
	h.publishIfActive(ne)
}

// mergeEvent deals with events to a merge node
func (h *Hub) mergeEvent(run *Run, node mergeNode, e event.Event) {
	log.Debugf("<%s> (%s) - merge %s", run.Ref.FlowRef, run.Ref.Run, e.Tag)

	waitsDone, opts := h.runs.updateWithMergeEvent(run, node.NodeRef().ID, e.Tag, e.Opts)

	// is the merge satisfied
	if (node.TypeOfNode() == "any" && waitsDone == 1) || // only fire an any merge once
		(node.TypeOfNode() == "all" && waitsDone == node.Waits()) {

		e := event.Event{
			RunRef:     run.Ref,
			SourceNode: node.NodeRef(),
			Tag:        node.GetTag(config.SubTagGood), // when merges fire they emit the good event
			Good:       true,
			Opts:       opts,
		}
		h.publishIfActive(e)
	}
}

// endRun marks and saves this run as being complete
func (h *Hub) endRun(run *Run, source config.NodeRef, opts nt.Opts, status string, good bool) {
	log.Debugf("<%s> - END RUN (status:%s, good:%v)", run.Ref, status, good)
	didEndIt := h.runs.end(run, status, good)
	// if this end call was not the one that actually ended it then dont publish the end event
	if !didEndIt {
		return
	}

	// publish specific end run event - so other observers know specifically that this flow finished
	e := event.Event{
		RunRef:     run.Ref,
		SourceNode: source,
		Tag:        tagEndFlow,
		Opts:       opts,
	}
	h.queue.Publish(e)
}

func (h *Hub) setupHosts(adminTok string) {
	h.Lock()
	defer h.Unlock()
	// TODO - consider host discovery via various mechanisms
	// e.g. etcd, dns, env vars or direct k8s api
	for _, hostAddr := range h.config.Common.Hosts {
		log.Debug("connecting to host", hostAddr)
		addr := hostAddr + h.config.Common.BaseURL
		h.hosts = append(h.hosts, client.New(addr, adminTok))
	}
}

func anyTags(set, subset []string) bool {
	for _, t := range subset {
		for _, ht := range set {
			if t == ht {
				return true
			}
		}
	}
	return false
}
