import { useState } from "react";

import { DataTable } from "@/components/ui/DataTable";
import { Eyebrow } from "@/components/ui/Eyebrow";
import { LoadMore } from "@/components/ui/LoadMore";
import { PageHeader } from "@/components/ui/PageHeader";
import { Panel } from "@/components/ui/Panel";
import { QueryBoundary } from "@/components/ui/QueryBoundary";
import { Stack } from "@/components/layout/AppShell";
import { ProjectTable } from "@/components/memory/ProjectTable";
import { ScopeFilter, type ScopeSelection } from "@/components/memory/ScopeFilter";
import { DOMAINS, type DomainDefinition } from "@/components/memory/domains";
import { useProjects, useStats } from "@/api/queries";
import { useBrowse } from "@/api/useBrowse";
import { useScopeNames } from "@/api/useScopeNames";
import { cn } from "@/lib/cn";
import type { DomainTotals } from "@/api/types";

export function MemoryView() {
  const [domainId, setDomainId] = useState<string>(DOMAINS[0].id);
  const [scope, setScope] = useState<ScopeSelection | null>(null);

  const stats = useStats(scope);
  const projects = useProjects();
  const domain = DOMAINS.find((entry) => entry.id === domainId) ?? DOMAINS[0];

  return (
    <>
      <PageHeader
        accent="knows"
        lede="Facts are claims it can restate. Experiences are things that happened, folded upward as they age. The rest is the machinery underneath."
      >
        What it currently
      </PageHeader>

      <Stack>
        <div className="flex flex-col gap-3">
          <ScopeFilter selected={scope} onChange={setScope} />

          <nav className="flex flex-wrap gap-1.5" aria-label="Memory domains">
            {DOMAINS.map((entry) => (
              <DomainTab
                key={entry.id}
                active={entry.id === domainId}
                count={countFor(entry.id, stats.data?.totals)}
                onClick={() => setDomainId(entry.id)}
              >
                {entry.label}
              </DomainTab>
            ))}
          </nav>
        </div>

        {/* Remounted per domain and per scope so paging state never leaks
            from one list into another. */}
        <DomainPanel
          key={`${domain.id}-${scope ? `${scope.user}/${scope.project}` : "all"}`}
          domain={domain}
          scope={scope}
        />

        <section>
          <Eyebrow count={stats.data ? `${stats.data.totals.projects}` : undefined}>
            Projects
          </Eyebrow>
          <Panel>
            <QueryBoundary query={projects} skeletonHeight={220}>
              {(page) => <ProjectTable projects={page.items} />}
            </QueryBoundary>
          </Panel>
        </section>
      </Stack>
    </>
  );
}

interface DomainPanelProps {
  domain: DomainDefinition<never>;
  scope: ScopeSelection | null;
}

function DomainPanel({ domain, scope }: DomainPanelProps) {
  const names = useScopeNames();
  const browse = useBrowse<never>(domain.route, scope ? { ...scope } : {});

  return (
    <Panel>
      <QueryBoundary query={browse.query} skeletonHeight={240}>
        {() => (
          <>
            <DataTable
              rows={browse.rows}
              columns={domain.columns(names)}
              getRowKey={domain.rowKey}
              caption={`Stored ${domain.label.toLowerCase()}`}
              empty={domain.empty}
            />
            <LoadMore
              hasMore={browse.hasMore}
              loading={browse.loadingMore}
              onClick={browse.loadMore}
              shown={browse.rows.length}
            />
          </>
        )}
      </QueryBoundary>
    </Panel>
  );
}

/**
 * Counts come from /v1/stats for the selected scope, so a tab and the list
 * beneath it always describe the same set of rows.
 */
function countFor(domainId: string, totals: DomainTotals | undefined): number | undefined {
  if (!totals) return undefined;
  const key = domainId === "working" ? "working_memory" : domainId;
  return (totals as unknown as Record<string, number>)[key];
}

interface DomainTabProps {
  active: boolean;
  count: number | undefined;
  onClick: () => void;
  children: React.ReactNode;
}

function DomainTab({ active, count, onClick, children }: DomainTabProps) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={cn(
        "flex cursor-pointer items-center gap-2 rounded-lg border px-3 py-1.5",
        "text-[13.5px] font-medium -tracking-[0.1px] transition-colors",
        active
          ? "border-border-bright bg-surface-2 text-text"
          : "border-border bg-surface text-text-3 hover:border-border-2 hover:text-text-2",
      )}
    >
      {children}
      {count !== undefined ? (
        <span className={cn("data", count === 0 ? "text-text-5" : "text-text-4")}>{count}</span>
      ) : null}
    </button>
  );
}
