import { clsx, type ClassValue } from 'clsx';
import { twMerge } from 'tailwind-merge';

// cn：合并 Tailwind 类名（clsx 处理条件，twMerge 去冲突）。
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
