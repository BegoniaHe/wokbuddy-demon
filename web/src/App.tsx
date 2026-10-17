import { useEffect, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { api } from '@/lib/api';
import { Toaster, toast } from '@/components/Toaster';
import { useTheme } from '@/hooks/useTheme';
import { useHashRoute } from '@/hooks/useHashRoute';
import { setUnauthorizedHandler } from '@/lib/api';
import { Shell } from '@/components/Shell';
import { KeyGate } from '@/components/KeyGate';
import { AddAccountDialog } from '@/components/AddAccountDialog';
import { VIEWS, type ViewId } from '@/views/registry';
import { AccountsView } from '@/views/AccountsView';
import { UsageView } from '@/views/UsageView';
import { PackagesView } from '@/views/PackagesView';
import { TasksCenterView } from '@/views/TasksCenterView';
import { ModelsView } from '@/views/ModelsView';
import { KeysView } from '@/views/KeysView';
import { ConfigView } from '@/views/ConfigView';
import { LogsView } from '@/views/LogsView';
import { AgentsView } from '@/views/AgentsView';

export default function App() {
  const [view, go] = useHashRoute<ViewId>(VIEWS, 'accounts');
  const { effective, toggle } = useTheme();
  const [keyOpen, setKeyOpen] = useState(false);
  const [addOpen, setAddOpen] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const qc = useQueryClient();

  useEffect(() => {
    setUnauthorizedHandler(() => setKeyOpen(true));
    return () => setUnauthorizedHandler(null);
  }, []);

  const onRefresh = async () => {
    setRefreshing(true);
    try {
      await api('balance_all', { method: 'POST' });
      await qc.invalidateQueries({ queryKey: ['overview'] });
      toast('余额已从上游刷新', 'ok');
    } catch (e) {
      toast('刷新失败：' + (e as Error).message, 'err');
      await qc.invalidateQueries({ queryKey: ['overview'] });
    } finally {
      setRefreshing(false);
    }
  };

  const content = (() => {
    switch (view) {
      case 'accounts':
        return <AccountsView />;
      case 'usage':
        return <UsageView />;
      case 'packages':
        return <PackagesView />;
      case 'taskscenter':
        return <TasksCenterView />;
      case 'models':
        return <ModelsView />;
      case 'keys':
        return <KeysView />;
      case 'config':
        return <ConfigView />;
      case 'agents':
        return <AgentsView />;
      case 'logs':
        return <LogsView />;
    }
  })();

  return (
    <>
      <Shell
        view={view}
        onNavigate={go}
        theme={effective}
        onToggleTheme={toggle}
        onAdd={() => setAddOpen(true)}
        onRefresh={() => void onRefresh()}
        refreshing={refreshing}
      >
        {content}
      </Shell>
      <KeyGate open={keyOpen} onOpenChange={setKeyOpen} />
      <AddAccountDialog open={addOpen} onOpenChange={setAddOpen} />
      <Toaster />
    </>
  );
}
