// @vitest-environment jsdom
// 这两个文件要读写 localStorage / 渲染 hook：默认 environment 是 node，逐文件切到
// jsdom（jsdom 已是 devDependency，不改全局配置以免拖慢其它纯函数用例）。
import { act, renderHook } from '@testing-library/react';
import { beforeEach, describe, expect, it } from 'vitest';
import { readLocalFlag, useLocalFlag, writeLocalFlag } from './useLocalFlag';

const KEY = 'wb2api.test.flag';

describe('readLocalFlag / writeLocalFlag', () => {
  beforeEach(() => localStorage.clear());

  it('未设置时返回默认值', () => {
    expect(readLocalFlag(KEY, true)).toBe(true);
    expect(readLocalFlag(KEY, false)).toBe(false);
  });
  it('写入后可读回', () => {
    writeLocalFlag(KEY, true);
    expect(readLocalFlag(KEY, false)).toBe(true);
    writeLocalFlag(KEY, false);
    expect(readLocalFlag(KEY, true)).toBe(false);
  });
  it('非法值回落默认值', () => {
    localStorage.setItem(KEY, 'yes');
    expect(readLocalFlag(KEY, false)).toBe(false);
  });
});

describe('useLocalFlag', () => {
  beforeEach(() => localStorage.clear());

  it('默认展开（true），翻转后写回本机', () => {
    const { result } = renderHook(() => useLocalFlag('wb2api.expfold.v1', true));

    expect(result.current[0]).toBe(true);

    act(() => result.current[1]());

    expect(result.current[0]).toBe(false);
    expect(readLocalFlag('wb2api.expfold.v1', true)).toBe(false);
  });

  it('显式设值时按入参写入', () => {
    const { result } = renderHook(() => useLocalFlag(KEY, false));

    act(() => result.current[1](true));

    expect(result.current[0]).toBe(true);
    expect(readLocalFlag(KEY, false)).toBe(true);
  });
});
