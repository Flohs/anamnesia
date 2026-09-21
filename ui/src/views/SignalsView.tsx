import { Eyebrow } from "@/components/ui/Eyebrow";
import { PageHeader } from "@/components/ui/PageHeader";
import { Panel, PanelHeader } from "@/components/ui/Panel";
import { QueryBoundary } from "@/components/ui/QueryBoundary";
import { Stack } from "@/components/layout/AppShell";
import { FailedSources } from "@/components/activity/FailedSources";
import { HookRunTable } from "@/components/activity/HookRunTable";
import { HealthAlerts } from "@/views/HealthAlerts";
import { DataTable, type Column } from "@/components/ui/DataTable";
import { useConfig, useHealth, useSources, useStats } from "@/api/queries";
import type { ActivitySnapshot, ConfigEntry, ConnectionState } from "@/api/types";
import { useHookRuns } from "@/api/queries";

interface SignalsViewProps {
  snapshot: ActivitySnapshot | null;
  connection: ConnectionState;
}

const CONFIG_COLUMNS: ReadonlyArray<Column<ConfigEntry>> = [
  {
    id: "key",
    header: "setting",
    numeric: true,
    render: (row) => <span className="text-text-2">{row.key}</span>,
  },
  {
    id: "value",
    header: "value",
    numeric: true,
    render: (row) => (row.secret ? <span className="text-text-5">masked</span> : row.value),
  },
  { id: "source", header: "from", numeric: true, render: (row) => row.source },
];

export function SignalsView({ snapshot, connection }: SignalsViewProps) {
  const health = useHealth();
  const hooks = useHookRuns();
  const stats = useStats();
  const config = useConfig();
  const failed = useSources("failed");

  const failedSources = stats.data?.sources_by_state.failed ?? 0;

  return (
    <>
      <PageHeader accent="absent" lede="A memory service quietly doing nothing looks exactly like one with nothing to do. These are the states that tell the two apart.">
        When something is wrong, or simply
      </PageHeader>

      <Stack>
        <HealthAlerts health={health.data} snapshot={snapshot} connection={connection} />

        <section>
          <Eyebrow count="from hooks.log · survives restarts">Hook runs</Eyebrow>
          <Panel>
            <QueryBoundary query={hooks} skeletonHeight={240}>
              {(data) => <HookRunTable runs={data.items} />}
            </QueryBoundary>
          </Panel>
        </section>

        <section>
          <Eyebrow count={failedSources > 0 ? `${failedSources}` : undefined}>
            Failed sources
          </Eyebrow>
          <Panel>
            <QueryBoundary query={failed} skeletonHeight={160}>
              {(page) => <FailedSources sources={page.items} />}
            </QueryBoundary>
          </Panel>
        </section>

        <section>
          <Eyebrow count="secrets are masked by the server">Configuration</Eyebrow>
          <Panel>
            <PanelHeader title="setting" meta="read-only" />
            <QueryBoundary query={config} skeletonHeight={280}>
              {(data) => (
                <DataTable
                  rows={data.items}
                  columns={CONFIG_COLUMNS}
                  getRowKey={(row) => row.key}
                  caption="Resolved configuration"
                />
              )}
            </QueryBoundary>
          </Panel>
        </section>
      </Stack>
    </>
  );
}
