import { apiClient } from './client';
import type {
  PromptDetail,
  PromptTemplate,
  PromptTemplatesResponse,
  PromptUsersResponse,
} from '@/types/prompts';

const PROMPTS_TIMEOUT_MS = 30 * 1000;

export const promptsApi = {
  listUsers: () =>
    apiClient.get<PromptUsersResponse>('/prompts/users', {
      timeout: PROMPTS_TIMEOUT_MS,
    }),

  /**
   * Fetch prompt detail. Pass `inlineTemplates: true` to have the server
   * splice template bodies back into each message's `prompt` (no need for
   * a second round-trip). Default false keeps the response small.
   */
  getDetail: (keyOrHash: string, opts?: { limit?: number; inlineTemplates?: boolean }) =>
    apiClient.get<PromptDetail>(`/prompts/users/${encodeURIComponent(keyOrHash)}`, {
      timeout: PROMPTS_TIMEOUT_MS,
      params: {
        ...(opts?.limit ? { limit: opts.limit } : {}),
        ...(opts?.inlineTemplates ? { inline_templates: 1 } : {}),
      },
    }),

  listTemplates: () =>
    apiClient.get<PromptTemplatesResponse>('/prompts/templates', {
      timeout: PROMPTS_TIMEOUT_MS,
    }),

  getTemplate: (hash: string) =>
    apiClient.get<PromptTemplate>(`/prompts/templates/${encodeURIComponent(hash)}`, {
      timeout: PROMPTS_TIMEOUT_MS,
    }),
};
