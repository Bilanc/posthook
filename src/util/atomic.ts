import { mkdir, rename, writeFile } from "node:fs/promises";
import { dirname } from "node:path";

export async function writeAtomic(path: string, content: string): Promise<void> {
  await mkdir(dirname(path), { recursive: true });
  const tmp = `${path}.${process.pid}.${Date.now()}.tmp`;
  await writeFile(tmp, content, { encoding: "utf8", mode: 0o644 });
  await rename(tmp, path);
}
