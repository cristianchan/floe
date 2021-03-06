import {Panel} from '../panel/panel.js';
import {el} from '../panel/panel.js';
import {Form} from '../panel/form.js';
import {RestCall} from '../panel/rest.js';
import {AttacheExpander} from '../panel/expander.js';
import {States} from '../panel/expander.js';
import {PrettyDate} from '../panel/util.js';
import {ToHHMMSS} from '../panel/util.js';
import {EmbellishSummary} from '../page/flow.js';

"use strict";

// the controller for the Dashboard
export function FlowSingle() {
    var panel;
    var dataReq = function(){
        return {
            URL: '/flows/' + panel.IDs[0] + '/runs/' + panel.IDs[1],
        };
    }

    var events = [];

    // panel is view - or part of it
    var panel = new Panel(this, null, graphFlow, '#main', events, dataReq);

    this.Map = function(evt, data) {
        var expState = States(panel.IDs[1]);
        console.log("flow got a call to Map", evt);
        if (evt.Type == 'rest') {
          var pl = evt.Value.Response.Payload;
          pl.Parent = '/flows/' + panel.IDs[0];
          pl.Summary = EmbellishSummary(pl.Summary);
          pl.Graph.forEach((r, i) => {
              r.forEach((nr, ni) => {
                nr.StartedAgo = "";
                if (nr.Started != "0001-01-01T00:00:00Z") {
                    var started = new Date(nr.Started)
                    nr.StartedAgo = PrettyDate(nr.Started);
                    var now = new Date();
                    nr.Took = "("+ToHHMMSS((now - started)/1000)+")";
                }
                nr.Took = "";
                if (nr.Stopped != "0001-01-01T00:00:00Z") {
                    var started = new Date(nr.Started)
                    var stopped = new Date(nr.Stopped);
                    nr.Took = "("+ToHHMMSS((stopped - started)/1000)+")";
                }
                nr.Expanded = expState[nr.ID];
                pl.Graph[i][ni] = nr;
              });
          });

          console.log(pl);

          return pl;
        }
        // ongoing web socket events...
        if (evt.Type == 'ws') {
            if (data == null) { // no need to update data that has not been initialised yet
                return;
            }
            var runID = evt.Msg.RunRef.Run.HostID + "-" + evt.Msg.RunRef.Run.ID;
            if (evt.Msg.RunRef.FlowRef.ID != panel.IDs[0] || runID != panel.IDs[1]) {
                console.log(evt.Msg.RunRef.FlowRef.ID, panel.IDs[0],  runID,  panel.IDs[1]);
                return;
            }

            if (evt.Msg.Tag == "sys.end.all") {
                data.Summary.Ended = true;
                var d = new Date();
                data.Summary.EndTime = d.toISOString();
                if (evt.Msg.Good) {
                    data.Summary.Status = "good";
                } else {
                    data.Summary.Status = "bad";
                }
                data.Summary = EmbellishSummary(data.Summary);

                // mark all data nodes disabled
                data.Graph.forEach((r, i) => {
                    r.forEach((nr, ni) => {
                        if (nr.Type == "data") {
                            nr.Enabled = false;
                        }
                    });
                });
                return data;
            }

            function activeStatus(type) {
                switch (type) {
                    case "merge":
                        return "waiting";
                    default:
                        return "running";
                }
            };
            
            var change = false;
            // find the node to which this event applies and update it
            data.Graph.forEach((r, i) => {
                r.forEach((nr, ni) => {
                    nr.Expanded = expState[nr.ID];
                    if (nr.ID != evt.Msg.SourceNode.ID) {
                        return;
                    }
                    change = true;
                    // state changes
                    if (evt.Msg.Tag == "sys.node.start") {
                        console.log("got sys node start or update");
                        // update the data and return it
                        var d = new Date();
                        nr.Started = d.toISOString();
                        nr.Status = activeStatus(nr.Class);
                    }
                    if (evt.Msg.Tag == "sys.node.update") {
                        nr.Status = activeStatus(nr.Class);
                        if (nr.Type == "exec") {
                            if (!nr.Logs) {
                                nr.Logs = [];
                            }
                            nr.Logs.push(evt.Msg.Opts.update);
                        }
                    }
                    if (evt.Msg.Tag == "sys.data.required") {
                        nr.Enabled = true;
                        nr.Status = "waiting";
                    }
                    if (
                        evt.Msg.Tag.startsWith("task") ||
                        evt.Msg.Tag.startsWith("merge")
                    ) {
                        console.log("got task or merge event", evt.Msg.Tag);
                        // task or data must have finished
                        var d = new Date();
                        nr.Stopped = d.toISOString();
                        nr.Status = "finished";
                        nr.Result = "success"; // TODO parse result
                        nr.Enabled = false;  // data nodes have submitted their data.
                        if (nr.Type == "data") {
                            nr.Fields = evt.Msg.Opts.form.fields;
                        }
                    }
                    if (nr.Started != "0001-01-01T00:00:00Z") {
                        nr.StartedAgo = PrettyDate(nr.Started);
                        var started = new Date(nr.Started);
                        var toTime = new Date();
                        if (nr.Stopped != "0001-01-01T00:00:00Z") {
                            toTime = new Date(nr.Stopped);
                        }
                        console.log("tt",toTime);
                        console.log("st",started);
                        nr.Took = "("+ToHHMMSS((toTime - started)/1000)+")";
                        console.log(nr.Took);
                    }
                    console.log("expanded", nr.ID, expState[nr.ID]);
                    data.Graph[i][ni] = nr;
                });
            });            
            if (change) {
                return data;
            }
        }
    }

    // TODO - dedupe with flow.js
    var sendData = function(data) { 
        var payload = {
            Ref: {
                ID:  panel.IDs[0],
                Ver: 1
            },
            Run: panel.IDs[1],
            Form: data
        }
        RestCall(panel.evtHub, "POST", "/push/data", payload);
    }

    // AfterRender is called when the panel rendered something.
    // we go and add the child summary panels
    this.AfterRender = function(data) {

        if (data == undefined) {
            return
        }
        // add all the forms
        data.Data.Graph.forEach((r, i) => {
            r.forEach((nr, ni) => {
                if (nr.Type == "data") {
                    if (nr.Enabled) {
                        console.log('draw editable form');

                        var form = {
                            ID: nr.ID,
                            fields: nr.Fields,
                        };
                        var formP = new Form('#expander-'+nr.ID, form, sendData);
                        formP.Activate();

                    } else {
                        console.log('draw uneditable values');
                    }
                }
            });
        });
        // make things expandable
        AttacheExpander(panel.IDs[1], el('triggers'));
        AttacheExpander(panel.IDs[1], el('tasks'));
    }

    return panel;
}

var graphFlow = `
    <div id='flow' class='flow-single'>
        <div class="crumb">
          <a href='{{=it.Data.Parent}}'>← back to {{=it.Data.FlowName}}</a>
        </div>
        <summary>
            <h2>{{=it.Data.Name}}</h2>
            <span class="label {{=it.Data.Summary.Status}}">{{=it.Data.Summary.Stat}}</span>
            <span>{{=it.Data.Summary.StartedAgo}}</span><span>{{=it.Data.Summary.Took}}</span>
        </summary>
        
        <divider></divider>

        <triggers>
          {{~it.Data.Triggers :trigger:index}}

          <box id='trig-{{=trigger.ID}}' class='trigger{{? !trigger.Enabled}} disabled{{?}}'>
            {{? trigger.Type=='data'}}
              <div for="{{=trigger.ID}}" class="data-title expander-ctrl">
                  <h4>{{=trigger.Name}}</h4><i class='icon-angle-circled-right'></i>
              </div>
            {{??}}
              <div class="data-title">
                  <h4>{{=trigger.Name}}</h4>
              </div>
            {{?}}
            
            {{? trigger.Type=='data'}}
              <detail id='expander-{{=trigger.ID}}' class='expander'>
                  {{~trigger.Fields :field:findex}}
                    <div id="field-{{=field.id}}", class='kvrow'>
                      <div class='prompt'>{{=field.prompt}}:</div> 
                      <div class='value'>{{=field.value}}</div>
                    </div>
                  {{~}}
              </detail>
            {{?}}
          </box>

          {{~}}
        </triggers>
        
        <divider></divider>
        
        <tasks>
        {{~it.Data.Graph :level:index}}
          <div id='level-{{=index}}' class='level'>
          {{~level :node:indx}}
            <box id='node-{{=node.ID}}' class='task {{=node.Result}} {{=node.Status}}'>
              <div for='{{=node.ID}}' class='data-title expander-ctrl'>
                  <h4>{{=node.Name}}</h4><i class='icon-angle-circled-right{{? node.Expanded}} open{{?}}'></i>
                  {{?node.Status=="running"}}<img class="gear" src="/static/img/gear.svg"><img>{{?}}
              </div>
              
              <detail id='expander-{{=node.ID}}' class='expander{{? node.Type!="data"}} show-some{{?}}{{? node.Expanded}} expand{{?}}'>
              {{? node.Type=="data"}}
                {{~node.Fields :field:findex}}
                <div id="field-{{=field.id}}", class='kvrow'>
                    <div class='prompt'>{{=field.prompt}}:</div> 
                    <div class='value'>{{=field.value}}</div>
                </div>
                {{~}}
              {{??}}
                <p class='ago'>{{=node.StartedAgo}}</p><p class='took'>{{=node.Took}}</p>
                {{? node.Class=="merge"}}
                <div class='clear'>
                    {{ for(var prop in node.Waits) { }}
                    <div>{{=prop}} {{=node.Waits[prop]}}</div>
                    {{ } }}
                </div>
                {{??}}
                    <code>
{{~node.Logs :line:lindex}}{{=line}}
{{~}}
                    </code>
                {{?}}
              {{?}}
              </detail>
            </box>
          {{~}}
          </div>
        {{~}}
        </tasks>
    </div>
`