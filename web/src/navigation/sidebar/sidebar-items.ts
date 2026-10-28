import {
  Activity,
  Ban,
  BellRing,
  Bot,
  Brain,
  Bug,
  ClipboardList,
  FolderOpen,
  FolderSync,
  LayoutDashboard,
  type LucideIcon,
  MessageSquare,
  Network,
  Plug,
  Radio,
  ScrollText,
  Settings2,
  ShieldAlert,
  Sparkles,
  Target,
  Terminal,
  Wrench,
} from "lucide-react";

export type NavBadge = "new" | "soon";

export interface NavSubItem {
  id: string;
  title: string;
  url: string;
  icon?: LucideIcon;
  badge?: NavBadge;
  disabled?: boolean;
  newTab?: boolean;
}

interface NavItemBase {
  id: string;
  title: string;
  icon?: LucideIcon;
  badge?: NavBadge;
  disabled?: boolean;
  newTab?: boolean;
}

export interface NavMainLinkItem extends NavItemBase {
  url: string;
  subItems?: never;
}

export interface NavMainParentItem extends NavItemBase {
  subItems: NavSubItem[];
}

export type NavMainItem = NavMainLinkItem | NavMainParentItem;

export interface NavGroup {
  id: number;
  label?: string;
  items: NavMainItem[];
}

export const sidebarItems: NavGroup[] = [
  {
    id: 1,
    label: "Operação",
    items: [
      { id: "dashboard", title: "Painel", url: "/dashboard", icon: LayoutDashboard },
      { id: "chat", title: "Conversa", url: "/chat", icon: MessageSquare },
      { id: "tasks", title: "Tarefas", url: "/function/tasks", icon: Target },
      { id: "findings", title: "Achados", url: "/function/findings", icon: Bug },
      { id: "traffic", title: "Tráfego", url: "/function/traffic", icon: Activity },
      { id: "commands", title: "Execuções", url: "/function/commands", icon: Terminal },
      { id: "llm-records", title: "Registros de LLM", url: "/function/llm-records", icon: Radio },
      { id: "assets", title: "Ativos", url: "/function/assets", icon: Network },
      { id: "sync", title: "Sincronizar ativos", url: "/function/sync", icon: FolderSync },
      { id: "workspace", title: "Espaço de trabalho", url: "/function/workspace", icon: FolderOpen },
    ],
  },
  {
    id: 2,
    label: "Sistema",
    items: [
      { id: "llm", title: "LLM", url: "/system/llm", icon: Brain },
      { id: "agents", title: "Agentes", url: "/system/agents", icon: Bot },
      { id: "mcp", title: "MCP", url: "/system/mcp", icon: Plug },
      { id: "skills", title: "Skills", url: "/system/skills", icon: Sparkles },
      { id: "tools", title: "Ferramentas", url: "/system/tools", icon: Wrench },
      { id: "notify", title: "Notificações", url: "/system/notify", icon: BellRing },
      { id: "intercept", title: "Regras de interceptação", url: "/system/intercept", icon: ShieldAlert },
      { id: "asset-intercept", title: "Bloqueio de ativos", url: "/system/intercept/assets", icon: Ban },
      { id: "approvals", title: "Aprovações", url: "/system/intercept/approvals", icon: ClipboardList },
      { id: "logs", title: "Logs", url: "/system/logs", icon: ScrollText },
      { id: "settings", title: "Configurações", url: "/system/settings", icon: Settings2 },
    ],
  },
];
