import {spawn} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {existsSync,mkdirSync} from 'node:fs';

const binary=fileURLToPath(new URL('./forge',import.meta.url));
if(!existsSync(binary)){
  const buildHome='/tmp/go-build-home';
  const buildCache='/tmp/go-build-cache';
  mkdirSync(buildHome,{recursive:true});
  mkdirSync(buildCache,{recursive:true});
  const build=spawn(process.env.GO_BIN??`${process.env.HOME??'/home/hatch'}/bin/go/bin/go`,['build','-o',binary,'.'],{stdio:'inherit',env:{...process.env,HOME:process.env.HOME??buildHome,GOCACHE:process.env.GOCACHE??buildCache}});
  build.once('error',error=>{console.error(error.message);process.exitCode=1});
  build.once('exit',(code,signal)=>{
    if(signal||code!==0){process.exitCode=code??1;return}
    launch();
  });
}else launch();

function launch(){
  const child=spawn(binary,[],{stdio:'inherit',env:process.env});
  child.once('error',error=>{console.error(error.message);process.exitCode=1});
  child.once('exit',(code,signal)=>{process.exitCode=signal?1:(code??1)});
  for(const signal of ['SIGTERM','SIGINT'] as const)process.once(signal,()=>child.kill(signal));
}
