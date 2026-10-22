import React from 'react';
import { LobeIcon } from '../../components/LobeIcon';
import { BoxOutlined } from '../../components/icons';

/**
 * Client → brand mark. Session-stat client names are the scanner's family
 * ids, so the table keys on those ids (including the pi family aliases);
 * anything unmapped keeps a neutral glyph rather than a wrong brand.
 */
const CLIENT_ICONS: Array<[RegExp, string]> = [
  [/^(?:pi|omp|kimchi|senpi|craft-agent)$/, 'Pi'],
  [/^claude(?:-code|-mirror)?$/, 'Claude'],
  [/^codex$/, 'Codex'],
  [/^copilot-vscode$/, 'Copilot'],
  [/^gemini(?:-cli)?$/, 'Gemini'],
  [/^qwen/, 'Qwen'],
  [/^kimi/, 'Kimi'],
  [/^dsh$/, 'DeepSeek'],
  [/^opencode(?:-review)?$/, 'OpenCode'],
  [/^amp$/, 'Amp'],
  [/^cherrystudio$/, 'CherryStudio'],
  [/^codebuddy$/, 'CodeBuddy'],
  [/^hermes$/, 'HermesAgent'],
  [/^goose$/, 'Goose'],
  [/^junie$/, 'Junie'],
  [/^grok/, 'Grok'],
  [/^cline$/, 'Cline'],
  [/^roocode$/, 'RooCode'],
  [/^kilo(?:code)?$/, 'KiloCode'],
  [/^lmstudio$/, 'LmStudio'],
  [/^unsloth$/, 'Unsloth'],
  [/^windsurf$/, 'Windsurf'],
  [/^trae$/, 'Trae'],
];

export const ClientMark: React.FC<{ client: string; size?: number }> = ({ client, size = 16 }) => {
  const iconId = CLIENT_ICONS.find(([pattern]) => pattern.test(client.toLowerCase()))?.[1];
  if (iconId) return <LobeIcon iconId={iconId} size={size} />;
  return (
    <span
      style={{ display: 'inline-flex', width: size, height: size, alignItems: 'center', justifyContent: 'center', color: 'var(--muted)', opacity: 0.7 }}
      aria-hidden="true"
    >
      <BoxOutlined size={size} strokeWidth={1.5} />
    </span>
  );
};
