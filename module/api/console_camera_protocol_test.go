package api

import (
	"context"
	"testing"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func TestConsoleProtocolPublishSharesCameraAndMicrophone(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		for _, protocol := range []string{"sip", "gb28181", "whip"} {
			var result struct {
				Camera, Microphone, Preview bool
				Source                      string
			}
			err := chromedp.Run(ctx, chromedp.Evaluate(`(function(protocol) {
				document.getElementById("publish-overlay").classList.add("active");
				document.getElementById("publish-protocol").value = protocol;
				updatePublishProtocol(true);
				function visible(id) { return document.getElementById(id).getClientRects().length > 0; }
				return {Camera:visible("publish-video-device"), Microphone:visible("publish-audio-device"), Preview:visible("publish-preview"), Source:document.getElementById("preflight-source-label").textContent};
			})(`+`"`+protocol+`"`+`)`, &result))
			if err != nil {
				t.Fatal(err)
			}
			if !result.Camera || !result.Microphone || !result.Preview || result.Source != "Camera + microphone" {
				t.Errorf("%s lacks real-device controls/preview: %+v", protocol, result)
			}
		}
	})
}

func TestConsoleCameraPublishLifecycle(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var failures []string
		err := chromedp.Run(ctx, chromedp.Evaluate(`(async function() {
			var failures = [], saved = {fetch:fetch, PC:RTCPeerConnection, caps:RTCRtpSender.getCapabilities, preview:startPreview};
			function assert(ok, message) { if (!ok) failures.push(message); }
			function delay() { return new Promise(function(resolve) { setTimeout(resolve, 10); }); }
			async function until(check) { for (var i=0; i<200; i++) { if (check()) return; await delay(); } throw new Error("lifecycle wait timed out"); }
			function deferred() { var resolve; var promise = new Promise(function(r) { resolve = r; }); return {promise:promise, resolve:resolve}; }
			function response(data, status) { return new Response(JSON.stringify({data:data}), {status:status || 200, headers:{"Content-Type":"application/json"}}); }
			var requests, tracks, peers, pending, stage, sourceReads, serial = 0;
			function reset(nextStage) {
				stopPublish(); stage=nextStage; requests=[]; tracks=[]; peers=[]; sourceReads=0; pending=deferred();
				managementRole="admin"; consoleSessionExpired=false; publishEndpointsReady=true;
				endpoints.webrtc=location.host; endpointSchemes.webrtc="http";
				document.getElementById("publish-protocol").value="sip";
				document.getElementById("publish-sip-device").value="browser-camera";
				document.getElementById("publish-sip-stream").value="sip/lifecycle";
				document.getElementById("publish-sip-codec").value="PCMA";
			}
			try {
				RTCRtpSender.getCapabilities=function(kind) { return {codecs:[{mimeType:kind === "video" ? "video/H264" : "audio/PCMA"}]}; };
				window.RTCPeerConnection=function() {
					var pc=new EventTarget(); pc.iceGatheringState="complete"; pc.iceConnectionState="connected"; pc.connectionState="connected"; pc.transceivers=[];
					pc.addTrack=function(track) { var sender={track:track}; pc.transceivers.push({sender:sender, setCodecPreferences:function(){}}); return sender; };
					pc.getTransceivers=function(){return pc.transceivers;}; pc.createOffer=async function(){return {type:"offer",sdp:"offer"};};
					pc.setLocalDescription=async function(offer){pc.localDescription=offer;}; pc.setRemoteDescription=async function(){};
					pc.close=function(){pc.connectionState="closed";}; pc.getStats=async function(){return new Map();}; peers.push(pc); return pc;
				};
				startPreview=async function() {
					var current=["video","audio"].map(function(kind){var t=new EventTarget();t.kind=kind;t.readyState="live";t.stop=function(){t.readyState="ended";};tracks.push(t);return t;});
					publishStream={getTracks:function(){return current;},getVideoTracks:function(){return [current[0]];},getAudioTracks:function(){return [current[1]];}};
				};
				window.fetch=async function(url, options) {
					options=options || {}; var method=options.method || "GET"; requests.push({url:String(url),method:method});
					if (method === "DELETE") return response({});
					if (String(url).includes("/webrtc/whip/")) {
						if (stage === "whip-pending") return pending.promise;
						return new Response("answer",{status:201,headers:{Location:"/webrtc/session/current-"+(++serial)}});
					}
					if (String(url).includes("/api/v1/streams/")) {
						sourceReads++;
						if (stage === "source-pending") return new Promise(function(resolve,reject){options.signal.addEventListener("abort",function(){reject(new DOMException("Aborted","AbortError"));},{once:true});});
						if (sourceReads===1) return response({},404);
						if (sourceReads===2) return response({publisher:"",stats:{video_frames:0,audio_frames:0}});
						return response({publisher:"capture",video_codec:"H264",audio_codec:"G711A",stats:{video_frames:1,audio_frames:1}});
					}
					if (method === "POST") {
						if (stage === "lab-pending") return pending.promise;
						if (stage === "lab-failed") return response({},503);
						return response({session:{id:"current-lab-"+serial,state:"active"}},201);
					}
					return response({sessions:[]});
				};
				reset("ready"); await startPublish();
				assert(sourceReads>=3 && !!publishLabSessionID,"protocol started before captured source audio/video arrived");
				stopPublish(); assert(tracks.every(function(t){return t.readyState==="ended";}),"Stop leaked capture tracks");
				reset("ready"); document.getElementById("publish-protocol").value="whip";
				document.getElementById("publish-stream-key").value="live/camera-\u00e9"; await startPublish();
				assert(!!publishPC && requests.some(function(r){return r.method==="POST" && r.url.includes("/webrtc/whip/live/"+encodeURIComponent("camera-\u00e9"));}),"WHIP lost support for existing non-ASCII stream keys");
				for (var lateStage of ["whip-pending","lab-pending"]) {
					reset(lateStage); var oldRun=startPublish();
					await until(function(){return requests.some(function(r){return r.method==="POST" && r.url.includes(lateStage==="whip-pending" ? "/webrtc/whip/" : "/lab/sessions");});});
					var oldPending=pending, oldTracks=tracks.slice(); stopPublish(); stage="ready"; await startPublish();
					var currentPC=publishPC, currentID=publishLabSessionID;
					oldPending.resolve(lateStage==="whip-pending" ? new Response("answer",{status:201,headers:{Location:"/webrtc/session/late"}}) : response({session:{id:"late-lab",state:"active"}},201));
					await oldRun;
					assert(publishPC===currentPC && publishLabSessionID===currentID && currentPC.connectionState!=="closed",lateStage+" response damaged newer run");
					assert(requests.some(function(r){return r.method==="DELETE" && r.url.endsWith(lateStage==="whip-pending" ? "/late" : "/late-lab");}),lateStage+" leaked late resource");
					assert(oldTracks.every(function(t){return t.readyState==="ended";}),lateStage+" leaked old tracks");
				}
				reset("lab-failed"); await startPublish();
				assert(!publishPC && !publishLabSessionID && tracks.every(function(t){return t.readyState==="ended";}),"failed protocol setup leaked resources");
				assert(requests.some(function(r){return r.method==="DELETE" && r.url.includes("/webrtc/session/");}),"failed protocol setup leaked WHIP session");
				reset("source-pending"); var cancelled=startPublish(); await until(function(){return sourceReads>0;}); stopPublish(); await cancelled;
				assert(!requests.some(function(r){return r.method==="POST" && r.url.includes("/lab/sessions");}),"Stop during source wait started protocol");
				reset("ready"); await startPublish(); await until(function(){return !publishPC;});
				assert(tracks.every(function(t){return t.readyState==="ended";}),"terminal protocol monitor leaked capture");
				reset("ready"); RTCRtpSender.getCapabilities=function(){return {codecs:[]};}; await startPublish();
				assert(requests.length===0 && peers.length===0 && document.getElementById("publish-status").textContent.includes("cannot publish"),"unsupported codec created resources or silently fell back");
				return failures;
			} finally { stopPublish(); window.fetch=saved.fetch; window.RTCPeerConnection=saved.PC; RTCRtpSender.getCapabilities=saved.caps; startPreview=saved.preview; }
		})()`, &failures, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
		if err != nil {
			t.Fatal(err)
		}
		for _, failure := range failures {
			t.Error(failure)
		}
	})
}

func TestConsoleCameraPreviewIgnoresStalePermissionFailure(t *testing.T) {
	withConsoleBrowser(t, func(ctx context.Context) {
		var status string
		err := chromedp.Run(ctx, chromedp.Evaluate(`(async function(){
			var old=navigator.mediaDevices.getUserMedia, reject;
			try {
				navigator.mediaDevices.getUserMedia=function(){return new Promise(function(resolve,r){reject=r;});};
				var pending=startPreview(); stopPublish(); setPublishStatus("New run"); reject(new Error("old denial")); await pending;
				return document.getElementById("publish-status").textContent;
			} finally {navigator.mediaDevices.getUserMedia=old;}
		})()`, &status, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }))
		if err != nil {
			t.Fatal(err)
		}
		if status != "New run" {
			t.Fatalf("stale permission failure replaced current status: %q", status)
		}
	})
}
