import fs from 'node:fs';
import path from 'node:path';

const [asarPath, unpackedRoot, outDir] = process.argv.slice(2);

const fd = fs.openSync(asarPath, 'r');
const head = Buffer.alloc(16);
fs.readSync(fd, head, 0, 16, 0);
const headerSize = head.readUInt32LE(4);
const jsonLen = head.readUInt32LE(12);
const jb = Buffer.alloc(jsonLen);
fs.readSync(fd, jb, 0, jsonLen, 16);
const header = JSON.parse(jb.toString('utf8'));
const dataStart = 8 + headerSize;

let packed = 0;
let unpacked = 0;
let linked = 0;
let missing = 0;

function walk(node, rel) {
  for (const [name, entry] of Object.entries(node.files || {})) {
    const p = rel ? `${rel}/${name}` : name;
    if (entry.files) {
      fs.mkdirSync(path.join(outDir, p), { recursive: true });
      walk(entry, p);
      continue;
    }
    const dest = path.join(outDir, p);
    fs.mkdirSync(path.dirname(dest), { recursive: true });
    if (entry.link) {
      fs.symlinkSync(entry.link, dest);
      linked++;
      continue;
    }
    if (entry.unpacked) {
      const src = path.join(unpackedRoot, p);
      if (fs.existsSync(src)) {
        fs.copyFileSync(src, dest);
        unpacked++;
      } else {
        missing++;
      }
      continue;
    }
    const buf = Buffer.alloc(entry.size);
    fs.readSync(fd, buf, 0, entry.size, dataStart + Number(entry.offset));
    fs.writeFileSync(dest, buf);
    packed++;
  }
}

fs.mkdirSync(outDir, { recursive: true });
walk(header, '');
fs.closeSync(fd);
console.log(JSON.stringify({ packed, unpacked, linked, missing }));
