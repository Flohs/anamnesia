import { useMemo } from "react";

import { useProjects, useUsers } from "@/api/queries";
import type { Scope } from "@/api/types";

/**
 * Turns the UUIDs the server puts on every row into the names a person reads.
 *
 * Memory rows carry `{user_id, project_id}`, while `/v1/projects` and
 * `/v1/users` carry the slugs and handles. Resolving that in one place keeps
 * every table from either showing raw UUIDs or fetching the directory itself.
 */
export interface ScopeNames {
  project: (scope: Scope) => string;
  user: (scope: Scope) => string;
  /** True once the directory has loaded; until then names fall back to ids. */
  ready: boolean;
}

/** A row with no project belongs to the user rather than to any repository. */
export const USER_LEVEL = "user-level";

export function useScopeNames(): ScopeNames {
  const projects = useProjects();
  const users = useUsers();

  return useMemo(() => {
    const projectById = new Map((projects.data?.items ?? []).map((p) => [p.id, p.slug]));
    const userById = new Map((users.data?.items ?? []).map((u) => [u.id, u.handle]));

    return {
      ready: projects.isSuccess && users.isSuccess,
      project: (scope) => {
        // Nullish rather than null: the server omits project_id entirely on a
        // user-level row instead of sending null, and an equality check
        // against null let undefined through to the id shortener.
        const projectId = scope.project_id ?? null;
        if (projectId === null) return USER_LEVEL;

        // An id with no match is shown truncated rather than hidden: a row
        // pointing at a project that no longer exists is worth noticing.
        return projectById.get(projectId) ?? shortId(projectId);
      },
      user: (scope) => userById.get(scope.user_id) ?? shortId(scope.user_id),
    };
  }, [projects.data, projects.isSuccess, users.data, users.isSuccess]);
}

function shortId(id: string): string {
  return id.slice(0, 8);
}
