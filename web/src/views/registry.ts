export const VIEWS = [
  'accounts',
  'usage',
  'packages',
  'taskscenter',
  'models',
  'keys',
  'config',
  'logs',
  'agents',
] as const;

export type ViewId = (typeof VIEWS)[number];

export const VIEW_TITLES: Record<ViewId, string> = {
  accounts: '账号池',
  usage: '用量',
  packages: '积分构成',
  taskscenter: '任务中心',
  models: '模型与档位',
  keys: '密钥',
  config: '配置',
  logs: '运行日志',
  agents: '智能体配置',
};
