import {spawn} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {existsSync} from 'node:fs';

const binary=fileURLToPath(new URL('./forge',import.meta.url));
if(!existsSync(binary)){
  console.error(`Required server binary is missing: ${binary}`);
  process.exit(1);
}

const child=spawn(binary,[],{stdio:'inherit',env:process.env});
child.once('error',error=>{console.error(error.message);process.exitCode=1});
child.once('exit',(code,signal)=>{process.exitCode=signal?1:(code??1)});
for(const signal of ['SIGTERM','SIGINT'] as const)process.once(signal,()=>child.kill(signal));
