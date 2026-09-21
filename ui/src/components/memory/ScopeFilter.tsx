import { useProjects } from "@/api/queries";
import { cn } from "@/lib/cn";

/**
 * A chosen scope. The user is carried alongside the project because the server
 * resolves a project name within one user, and answers 404 for a project that
 * belongs to another. Passing the slug alone made every project outside the
 * configured identity unopenable.
 */
export interface ScopeSelection {
  project: string;
  user: string;
}

interface ScopeFilterProps {
  selected: ScopeSelection | null;
  onChange: (selection: ScopeSelection | null) => void;
}

/**
 * Narrows every table to one project.
 *
 * Projects are listed in the order the directory returns them, with their row
 * counts, so choosing one is informed rather than a guess at which repository
 * holds anything.
 *
 * Only projects with something extracted are offered. A project that was
 * ingested but never extracted has sources and nothing else, so filtering to
 * it empties every table on the page at once; listing it invites a click that
 * can only disappoint. Sources are still visible elsewhere.
 */
export function ScopeFilter({ selected, onChange }: ScopeFilterProps) {
  const projects = useProjects();
  const items = (projects.data?.items ?? []).filter(
    (entry) => entry.counts.facts + entry.counts.experiences > 0,
  );

  // Checked after filtering: a lone "all projects" chip offers no choice.
  if (items.length === 0) return null;

  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <Option active={selected === null} onClick={() => onChange(null)}>
        all projects
      </Option>

      {items.map((entry) => {
        const total = entry.counts.facts + entry.counts.experiences + entry.counts.sources;
        return (
          <Option
            key={entry.id}
            active={selected?.project === entry.slug && selected.user === entry.user}
            onClick={() => onChange({ project: entry.slug, user: entry.user })}
          >
            {entry.slug}
            <span className="ml-1.5 text-text-5">{total}</span>
          </Option>
        );
      })}
    </div>
  );
}

interface OptionProps {
  active: boolean;
  onClick: () => void;
  children: React.ReactNode;
}

function Option({ active, onClick, children }: OptionProps) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={cn(
        "cursor-pointer rounded-sm border px-2 py-1 label transition-colors",
        active
          ? "border-coral/40 bg-coral/12 text-coral"
          : "border-border-2 bg-surface text-text-3 hover:border-border-bright hover:text-text-2",
      )}
    >
      {children}
    </button>
  );
}
