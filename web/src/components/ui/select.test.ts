import { describe, expect, it } from 'vitest';
import { selectOptionItems } from './select';

describe('selectOptionItems', () => {
  it('按顺序把选项数组转成 value → 标签映射', () => {
    const items = selectOptionItems([
      { value: '__all__', label: '全部结果' },
      { value: 'success', label: '成功' },
    ]);
    expect(items).toEqual({ __all__: '全部结果', success: '成功' });
  });

  it('空数组得到空映射', () => {
    expect(selectOptionItems([])).toEqual({});
  });
});
