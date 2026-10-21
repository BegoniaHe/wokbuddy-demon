// @vitest-environment jsdom
// 这两个文件要读写 localStorage / 渲染 hook：默认 environment 是 node，逐文件切到
// jsdom（jsdom 已是 devDependency，不改全局配置以免拖慢其它纯函数用例）。
import { beforeEach, describe, expect, it } from 'vitest';
import {
  clearRemark,
  readRemarks,
  remarkKey,
  remarkName,
  setRemark,
  writeRemarks,
} from './remarks';

const LS = 'wb2api.remarks.v1';

describe('remarkKey', () => {
  it('取 uid 前 8 位', () => {
    expect(remarkKey('0123456789abcdef')).toBe('01234567');
  });
  it('空值返回空串', () => {
    expect(remarkKey('')).toBe('');
    expect(remarkKey(null)).toBe('');
    expect(remarkKey(undefined)).toBe('');
  });
});

describe('readRemarks', () => {
  beforeEach(() => localStorage.clear());

  it('无数据返回空表', () => {
    expect(readRemarks()).toEqual({});
  });
  it('解析失败静默回落空表', () => {
    localStorage.setItem(LS, '{not json');
    expect(readRemarks()).toEqual({});
  });
  it('非对象（数组/标量）返回空表', () => {
    localStorage.setItem(LS, '[1,2]');
    expect(readRemarks()).toEqual({});
    localStorage.setItem(LS, '"x"');
    expect(readRemarks()).toEqual({});
  });
  it('过滤空值与非字符串', () => {
    localStorage.setItem(LS, JSON.stringify({ a: '  甲  ', b: '', c: 3 }));
    expect(readRemarks()).toEqual({ a: '甲' });
  });
});

describe('setRemark / clearRemark', () => {
  beforeEach(() => localStorage.clear());

  it('写入后可按键读回（去掉首尾空白）', () => {
    setRemark('u1-abcdefgh', '  主力号  ');
    expect(readRemarks()).toEqual({ 'u1-abcde': '主力号' });
  });
  it('空文本等价清除', () => {
    setRemark('u1-abcdefgh', '主力号');
    setRemark('u1-abcdefgh', '   ');
    expect(readRemarks()).toEqual({});
  });
  it('clearRemark 清除单条并保留其他条目', () => {
    setRemark('aaaa1111', '甲');
    setRemark('bbbb2222', '乙');
    clearRemark('aaaa1111');
    expect(readRemarks()).toEqual({ bbbb2222: '乙' });
  });
  it('writeRemarks 整体覆盖', () => {
    writeRemarks({ x: 'X' });
    expect(readRemarks()).toEqual({ x: 'X' });
  });
});

describe('remarkName', () => {
  beforeEach(() => localStorage.clear());

  it('无备注：display=原标题、ghost 为空', () => {
    expect(remarkName('u1-abcdefgh', '原昵称')).toEqual({ display: '原昵称', ghost: '' });
  });
  it('有备注：display=备注、ghost=原标题', () => {
    setRemark('u1-abcdefgh', '主力号');
    expect(remarkName('u1-abcdefgh', '原昵称')).toEqual({ display: '主力号', ghost: '原昵称' });
  });
  it('无昵称时回落短 uid', () => {
    expect(remarkName('u1-abcdefgh', '')).toEqual({ display: 'u1-abcde', ghost: '' });
  });
  it('空 uid 且无昵称 → —', () => {
    expect(remarkName('', '')).toEqual({ display: '—', ghost: '' });
  });
});
