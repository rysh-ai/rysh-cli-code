const {test} = require('node:test');
const assert = require('node:assert/strict');
const {readFileSync} = require('node:fs');
const vm = require('node:vm');
const script = readFileSync(__dirname+'/media.html','utf8').match(/<script>([\s\S]*)<\/script>/)[1];
function fixture(href='https://meeting.example/prefix/media#secret-capability') {
 const sockets=[], players=[], timers=[], intervals=[], revoked=[], elements={}, body={dataset:{}}, clock={now:0};
 const element=id=>elements[id]||(elements[id]={textContent:'',hidden:false});
 class Socket {
  static OPEN=1;
  constructor(url){this.url=String(url);this.readyState=1;this.sent=[];sockets.push(this);}
  send(v){this.sent.push(JSON.parse(v));}
  close(){this.readyState=3;this.onclose();}
 }
 class Audio {
  constructor(url){this.url=url;players.push(this);}
  play(){return new Promise((resolve,reject)=>{this.resolve=resolve;this.reject=reject;});}
  pause(){this.paused=true;}
  removeAttribute(){}
  load(){}
 }
 class MockURL extends URL {
  static createObjectURL(){return 'blob:test-'+players.length;}
  static revokeObjectURL(url){revoked.push(url);}
 }
 let removed=false;
 vm.runInNewContext(script, {
  location:(u=>({hash:u.hash,href,pathname:u.pathname}))(new URL(href)),
  history:{replaceState(){removed=true;}},document:{getElementById:element,body},
  URL:MockURL,WebSocket:Socket,Audio,Blob,performance:{now:()=>clock.now},
  setInterval(fn){intervals.push(fn);return intervals.length;},clearInterval(){},setTimeout(fn){timers.push(fn);},
 });
 if(sockets[0])sockets[0].onopen();
 return {sockets,players,timers,intervals,revoked,elements,body,clock,state:elements.state,removed};
}
// This file and media.html are byte-identical in rysh-cli and rysh-server.
const control=(socket,type,turn)=>socket.onmessage({data:JSON.stringify({type,turn})});
test('capability authenticates in frame, playback ack only on completion, blobs released',async()=>{
 const f=fixture(), s=f.sockets[0];
 assert.equal(f.removed,true);
 assert.equal(s.url,'wss://meeting.example/prefix/socket');
 assert.equal(s.sent[0].token,'secret-capability');
 await control(s,'audio','one');
 const playing=s.onmessage({data:new Uint8Array([1,2,3])});
 assert.equal(s.sent.length,1);
 f.players[0].resolve();await playing;
 assert.equal(s.sent.length,1);
 f.players[0].onended();
 assert.equal(s.sent[1].type,'played');
 assert.equal(s.sent[1].turn,'one');
 assert.equal(f.revoked.length,1);
});
test('clear cancels playback and late play rejection cannot cancel the next turn',async()=>{
 const f=fixture(), s=f.sockets[0];
 await control(s,'audio','old');
 const old=s.onmessage({data:new Uint8Array([1])});
 await control(s,'clear');
 assert.equal(f.players[0].paused,true);
 await control(s,'audio','new');
 const next=s.onmessage({data:new Uint8Array([2])});
 f.players[0].reject(Error('interrupted'));await old;
 assert.equal(s.sent.length,1);
 assert.equal(f.players[1].paused,undefined);
 f.players[1].resolve();await next;f.players[1].onended();
 assert.equal(s.sent[1].turn,'new');
});
test('disconnect stops audio and reconnect retains authentication',async()=>{
 const f=fixture(), s=f.sockets[0];
 await control(s,'audio','one');
 const playing=s.onmessage({data:new Uint8Array([1])});
 s.close();
 assert.equal(f.players[0].paused,true);
 f.players[0].resolve();await playing;
 assert.match(f.state.textContent,/Disconnected/);
 f.timers[0]();f.sockets[1].onopen();
 assert.equal(f.sockets[1].sent[0].token,'secret-capability');
});
const status=(socket,m)=>socket.onmessage({data:JSON.stringify(Object.assign({type:'status'},m))});
test('status messages drive the wall through a whole turn',async()=>{
 const f=fixture(), s=f.sockets[0], e=f.elements;
 assert.equal(f.body.dataset.state,'listening');
 assert.match(e.detail.textContent,/hey rysh/);
 assert.equal(e.turn.hidden,true);
 await status(s,{phase:'heard',state:'capturing',speaker:'Ada',request:'what changed today',elapsed_ms:1000});
 assert.equal(e.state.textContent,'Hearing your request');
 assert.equal(e.turn.hidden,false);
 assert.equal(e.speaker.textContent,'Ada');
 assert.equal(e.request.textContent,'“what changed today”');
 assert.equal(e.elapsed.textContent,'0:01');
 f.clock.now=64000;f.intervals.forEach(fn=>fn());
 assert.equal(e.elapsed.textContent,'1:05');
 await status(s,{phase:'thinking',state:'executing',speaker:'Ada',request:'what changed today',activity:'running agent docs',elapsed_ms:70000});
 assert.equal(e.state.textContent,'Thinking');
 assert.equal(e.detail.textContent,'running agent docs');
 assert.equal(e.elapsed.textContent,'1:10');
 await status(s,{phase:'preparing_speech',state:'synthesizing',speaker:'Ada',request:'what changed today',elapsed_ms:72000});
 assert.equal(e.state.textContent,'Preparing the answer');
 assert.equal(e.detail.textContent,'Turning the reply into speech');
 assert.equal(e.caption.hidden,true);
 await control(s,'audio','t1');
 const playing=s.onmessage({data:new Uint8Array([1])});
 await status(s,{phase:'speaking',state:'speaking',speaker:'Ada',request:'what changed today',caption:'Two commits landed. Both are merged!',elapsed_ms:73000});
 assert.equal(f.body.dataset.state,'speaking');
 assert.equal(e.caption.hidden,false);
 assert.equal(e.caption.textContent,'Two commits landed.');
 const p=f.players[0];p.duration=10;p.currentTime=8;f.intervals.forEach(fn=>fn());
 assert.equal(e.caption.textContent,'Both are merged!');
 p.resolve();await playing;p.onended();
 assert.equal(s.sent.at(-1).type,'played');
 assert.equal(f.body.dataset.state,'listening');
 assert.equal(e.turn.hidden,true);
 assert.equal(e.caption.hidden,true);
 assert.equal(e.caption.textContent,'');
});
test('unknown future states fall back to a generic label; errors and pause are readable',async()=>{
 const f=fixture(), s=f.sockets[0], e=f.elements;
 await status(s,{phase:'something_new',state:'something_new'});
 assert.equal(e.state.textContent,'Working');
 await status(s,{phase:'error',state:'failed',speaker:'Ada',request:'deploy'});
 assert.equal(f.body.dataset.state,'error');
 assert.match(e.state.textContent,/complete/);
 await status(s,{phase:'paused',state:'paused'});
 assert.equal(e.state.textContent,'Paused');
 assert.equal(e.turn.hidden,true);
});
test('text is rendered as text, never markup',async()=>{
 const f=fixture(), s=f.sockets[0], e=f.elements;
 await status(s,{phase:'heard',speaker:'<img src=x onerror=alert(1)>',request:'<b>hi</b>'});
 assert.equal(e.speaker.textContent,'<img src=x onerror=alert(1)>');
 assert.equal(e.speaker.innerHTML,undefined);
});
test('old daemons without status messages still get a sensible wall',async()=>{
 const f=fixture(), s=f.sockets[0];
 await control(s,'audio','one');
 assert.equal(f.state.textContent,'Speaking');
 await control(s,'clear');
 assert.equal(f.state.textContent,'Paused');
});
test('company page extras: company name, host allowlist, error text and audio captions',async()=>{
 const f=fixture(), s=f.sockets[0], e=f.elements;
 await status(s,{phase:'listening',company:'Acme',allow:'host'});
 assert.equal(e.brand.textContent,'Acme');
 assert.match(e.detail.textContent,/host/);
 await status(s,{phase:'error',company:'Acme',error:'Too many turns'});
 assert.equal(e.detail.textContent,'Too many turns');
 s.onmessage({data:JSON.stringify({type:'audio',turn:'t',caption:'Hello there.'})});
 assert.equal(e.caption.textContent,'Hello there.');
 s.close();
 assert.equal(e.brand.textContent,'Acme');
});
test('an attached agent is named on the wall',async()=>{
 const f=fixture(), s=f.sockets[0], e=f.elements;
 await status(s,{phase:'thinking',agent:'scribe',speaker:'Ada',request:'take notes'});
 assert.equal(e.detail.textContent,'Scribe is working on it');
 await status(s,{phase:'thinking',agent:'scribe',activity:'reading notes.md',speaker:'Ada',request:'take notes'});
 assert.equal(e.detail.textContent,'reading notes.md');
 await status(s,{phase:'speaking',agent:'scribe',speaker:'Ada',request:'take notes'});
 assert.equal(e.detail.textContent,'Scribe is answering');
});
test('rysh-server tile URL maps to its socket path',()=>{
 const f=fixture('https://rysh.ai/api/meeting-bot/m-1/media#cap123');
 assert.equal(f.sockets[0].url,'wss://rysh.ai/ws/meeting-bot/m-1/socket');
 assert.equal(f.sockets[0].sent[0].token,'cap123');
});
test('without a capability the page does not connect',()=>{
 const f=fixture('https://rysh.ai/api/meeting-bot/m-1/media');
 assert.equal(f.sockets.length,0);
 assert.equal(f.body.dataset.state,'error');
 assert.equal(f.elements.detail.textContent,'Missing session capability');
});
test('disconnect says it is reconnecting and never claims more',async()=>{
 const f=fixture(), s=f.sockets[0];
 s.close();
 assert.equal(f.elements.detail.textContent,'Reconnecting…');
});
test('an agent bot tells people to say hey <agent>',async()=>{
 const f=fixture(), s=f.sockets[0], e=f.elements;
 await status(s,{phase:'listening',agent:'scribe'});
 assert.match(e.detail.textContent,/hey scribe/);
 await status(s,{phase:'listening',agent:'scribe',allow:'host'});
 assert.match(e.detail.textContent,/host · say “hey scribe”/);
});
