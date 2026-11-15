// GraphQL v4: a small parser + executor for exactly the documents gh and
// github-mcp-server send (see sdk/conformance/checks-graphql.ts).
//
// Supported: queries and mutations, variables with defaults, aliases, inline
// input objects and enum lists, null variables, operation names, inline
// fragments, named fragments, batched arrays, __type/__schema introspection.
// Unknown fields answer HTTP 200 with an `errors` array naming the field;
// an unknown repository answers data.repository null with no errors.
import {
  store,
  VIEWER_LOGIN,
  VIEWER_NAME,
  VIEWER_EMAIL,
  nextSeq,
  nextGlobalId,
  globalId,
  parseGlobalId,
  type Repo,
  type Issue,
  type Pull,
} from './store.ts';
import { pullFiles, mergeConflict, squashMerge } from './rest.ts';
import { sendJson, parseJsonBody, isPlainObject, type Ctx } from './http-util.ts';

// ---------------------------------------------------------------------------
// Tokenizer
// ---------------------------------------------------------------------------

type TokenKind = 'name' | 'int' | 'float' | 'string' | 'punct' | 'eof';
interface Token {
  kind: TokenKind;
  value: string;
}

function tokenize(src: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < src.length) {
    const ch = src[i];
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < src.length && src[i] !== '\n') i += 1;
      continue;
    }
    if (ch === '"') {
      let value = '';
      i += 1;
      while (i < src.length && src[i] !== '"') {
        if (src[i] === '\\' && i + 1 < src.length) {
          const esc = src[i + 1];
          if (esc === 'n') value += '\n';
          else if (esc === 't') value += '\t';
          else if (esc === 'r') value += '\r';
          else value += esc;
          i += 2;
        } else {
          value += src[i];
          i += 1;
        }
      }
      i += 1; // closing quote
      tokens.push({ kind: 'string', value });
      continue;
    }
    if (/[0-9-]/.test(ch) && (ch !== '-' || /[0-9]/.test(src[i + 1] ?? ''))) {
      let num = '';
      let isFloat = false;
      if (ch === '-') {
        num += '-';
        i += 1;
      }
      while (i < src.length && /[0-9]/.test(src[i])) {
        num += src[i];
        i += 1;
      }
      if (src[i] === '.') {
        isFloat = true;
        num += '.';
        i += 1;
        while (i < src.length && /[0-9]/.test(src[i])) {
          num += src[i];
          i += 1;
        }
      }
      if (src[i] === 'e' || src[i] === 'E') {
        isFloat = true;
        num += src[i];
        i += 1;
        if (src[i] === '+' || src[i] === '-') {
          num += src[i];
          i += 1;
        }
        while (i < src.length && /[0-9]/.test(src[i])) {
          num += src[i];
          i += 1;
        }
      }
      tokens.push({ kind: isFloat ? 'float' : 'int', value: num });
      continue;
    }
    if (/[A-Za-z_]/.test(ch)) {
      let name = '';
      while (i < src.length && /[A-Za-z0-9_]/.test(src[i])) {
        name += src[i];
        i += 1;
      }
      tokens.push({ kind: 'name', value: name });
      continue;
    }
    if (src.startsWith('...', i)) {
      tokens.push({ kind: 'punct', value: '...' });
      i += 3;
      continue;
    }
    if ('{}()[]:!=$@'.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch });
      i += 1;
      continue;
    }
    throw new Error(`unexpected character ${JSON.stringify(ch)} in GraphQL document`);
  }
  tokens.push({ kind: 'eof', value: '' });
  return tokens;
}

// ---------------------------------------------------------------------------
// AST + parser
// ---------------------------------------------------------------------------

export type ValueNode =
  | { kind: 'variable'; name: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: ValueNode[] }
  | { kind: 'object'; fields: { name: string; value: ValueNode }[] };

export interface FieldNode {
  alias: string | null;
  name: string;
  args: { name: string; value: ValueNode }[];
  selection: SelectionNode[] | null;
}

export type SelectionNode =
  | { kind: 'field'; field: FieldNode }
  | { kind: 'fragmentSpread'; name: string }
  | { kind: 'inlineFragment'; typeCondition: string | null; selection: SelectionNode[] };

export interface OperationNode {
  kind: 'query' | 'mutation';
  name: string | null;
  variables: { name: string; defaultValue: ValueNode | null }[];
  selection: SelectionNode[];
}

export interface FragmentNode {
  name: string;
  typeCondition: string;
  selection: SelectionNode[];
}

export interface Document {
  operations: OperationNode[];
  fragments: Map<string, FragmentNode>;
}

class Parser {
  private pos = 0;
  private tokens: Token[];

  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token {
    return this.tokens[this.pos];
  }

  private next(): Token {
    return this.tokens[this.pos++];
  }

  private expect(value: string): Token {
    const token = this.next();
    if (token.value !== value) throw new Error(`expected ${JSON.stringify(value)}, got ${JSON.stringify(token.value)}`);
    return token;
  }

  private expectName(): string {
    const token = this.next();
    if (token.kind !== 'name') throw new Error(`expected a name, got ${JSON.stringify(token.value)}`);
    return token.value;
  }

  parseDocument(): Document {
    const doc: Document = { operations: [], fragments: new Map() };
    while (this.peek().kind !== 'eof') {
      const token = this.peek();
      if (token.kind === 'name' && token.value === 'fragment') {
        this.next();
        const name = this.expectName();
        this.expect('on');
        const typeCondition = this.expectName();
        const selection = this.parseSelectionSet();
        doc.fragments.set(name, { name, typeCondition, selection });
        continue;
      }
      doc.operations.push(this.parseOperation());
    }
    return doc;
  }

  private parseOperation(): OperationNode {
    let kind: 'query' | 'mutation' = 'query';
    let name: string | null = null;
    const token = this.peek();
    if (token.kind === 'name' && (token.value === 'query' || token.value === 'mutation')) {
      kind = this.next().value as 'query' | 'mutation';
      if (this.peek().kind === 'name') name = this.next().value;
    }
    const variables = this.peek().value === '(' ? this.parseVariableDefinitions() : [];
    const selection = this.parseSelectionSet();
    return { kind, name, variables, selection };
  }

  private parseVariableDefinitions(): { name: string; defaultValue: ValueNode | null }[] {
    this.expect('(');
    const vars: { name: string; defaultValue: ValueNode | null }[] = [];
    while (this.peek().value !== ')') {
      this.expect('$');
      const name = this.expectName();
      this.expect(':');
      this.parseType();
      let defaultValue: ValueNode | null = null;
      if (this.peek().value === '=') {
        this.next();
        defaultValue = this.parseValue();
      }
      vars.push({ name, defaultValue });
    }
    this.expect(')');
    return vars;
  }

  /** Skip a type expression: Name, [T], T! — only its shape matters here. */
  private parseType(): void {
    if (this.peek().value === '[') {
      this.next();
      this.parseType();
      this.expect(']');
    } else {
      this.expectName();
    }
    if (this.peek().value === '!') this.next();
  }

  private parseSelectionSet(): SelectionNode[] {
    this.expect('{');
    const selection: SelectionNode[] = [];
    while (this.peek().value !== '}') {
      if (this.peek().value === '...') {
        this.next();
        if (this.peek().kind === 'name' && this.peek().value === 'on') {
          this.next();
          const typeCondition = this.expectName();
          selection.push({ kind: 'inlineFragment', typeCondition, selection: this.parseSelectionSet() });
        } else if (this.peek().kind === 'name') {
          selection.push({ kind: 'fragmentSpread', name: this.next().value });
        } else {
          selection.push({ kind: 'inlineFragment', typeCondition: null, selection: this.parseSelectionSet() });
        }
        continue;
      }
      selection.push({ kind: 'field', field: this.parseField() });
    }
    this.expect('}');
    return selection;
  }

  private parseField(): FieldNode {
    let name = this.expectName();
    let alias: string | null = null;
    if (this.peek().value === ':') {
      this.next();
      alias = name;
      name = this.expectName();
    }
    const args: { name: string; value: ValueNode }[] = [];
    if (this.peek().value === '(') {
      this.next();
      while (this.peek().value !== ')') {
        const argName = this.expectName();
        this.expect(':');
        args.push({ name: argName, value: this.parseValue() });
      }
      this.expect(')');
    }
    // Directives (none in the checked documents) are skipped.
    while (this.peek().value === '@') {
      this.next();
      this.expectName();
      if (this.peek().value === '(') {
        this.next();
        let depth = 1;
        while (depth > 0) {
          const token = this.next();
          if (token.value === '(' || token.value === '[' || token.value === '{') depth += 1;
          if (token.value === ')' || token.value === ']' || token.value === '}') depth -= 1;
        }
      }
    }
    const selection = this.peek().value === '{' ? this.parseSelectionSet() : null;
    return { alias, name, args, selection };
  }

  private parseValue(): ValueNode {
    const token = this.next();
    if (token.value === '$') return { kind: 'variable', name: this.expectName() };
    if (token.kind === 'string') return { kind: 'string', value: token.value };
    if (token.kind === 'int') return { kind: 'int', value: Number.parseInt(token.value, 10) };
    if (token.kind === 'float') return { kind: 'float', value: Number.parseFloat(token.value) };
    if (token.value === '[') {
      const values: ValueNode[] = [];
      while (this.peek().value !== ']') values.push(this.parseValue());
      this.expect(']');
      return { kind: 'list', values };
    }
    if (token.value === '{') {
      const fields: { name: string; value: ValueNode }[] = [];
      while (this.peek().value !== '}') {
        const name = this.expectName();
        this.expect(':');
        fields.push({ name, value: this.parseValue() });
      }
      this.expect('}');
      return { kind: 'object', fields };
    }
    if (token.kind === 'name') {
      if (token.value === 'true') return { kind: 'boolean', value: true };
      if (token.value === 'false') return { kind: 'boolean', value: false };
      if (token.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: token.value };
    }
    throw new Error(`unexpected value token ${JSON.stringify(token.value)}`);
  }
}

function parseDocument(src: string): Document {
  return new Parser(tokenize(src)).parseDocument();
}

// ---------------------------------------------------------------------------
// Static schema (for field validation and introspection)
// ---------------------------------------------------------------------------

interface FieldDef {
  type: string;
  args?: Record<string, string>;
}
interface TypeDef {
  kind: 'OBJECT' | 'ENUM' | 'INPUT_OBJECT' | 'SCALAR';
  fields?: Record<string, FieldDef>;
  enumValues?: string[];
  inputFields?: Record<string, string>;
}

const SCHEMA: Record<string, TypeDef> = {
  Query: {
    kind: 'OBJECT',
    fields: {
      viewer: { type: 'User' },
      repository: { type: 'Repository', args: { owner: 'String!', name: 'String!' } },
      node: { type: 'Node', args: { id: 'ID!' } },
      __schema: { type: '__Schema' },
      __type: { type: '__Type', args: { name: 'String!' } },
    },
  },
  Mutation: {
    kind: 'OBJECT',
    fields: {
      createIssue: { type: 'CreateIssuePayload', args: { input: 'CreateIssueInput!' } },
      closeIssue: { type: 'CloseIssuePayload', args: { input: 'CloseIssueInput!' } },
      createPullRequest: { type: 'CreatePullRequestPayload', args: { input: 'CreatePullRequestInput!' } },
      mergePullRequest: { type: 'MergePullRequestPayload', args: { input: 'MergePullRequestInput!' } },
      addComment: { type: 'AddCommentPayload', args: { input: 'AddCommentInput!' } },
    },
  },
  User: {
    kind: 'OBJECT',
    fields: {
      login: { type: 'String!' },
      id: { type: 'ID!' },
      name: { type: 'String' },
      email: { type: 'String' },
    },
  },
  Repository: {
    kind: 'OBJECT',
    fields: {
      id: { type: 'ID!' },
      databaseId: { type: 'Int!' },
      name: { type: 'String!' },
      nameWithOwner: { type: 'String!' },
      owner: { type: 'User!' },
      defaultBranchRef: { type: 'Ref' },
      viewerPermission: { type: 'RepositoryPermission!' },
      hasIssuesEnabled: { type: 'Boolean!' },
      issue: { type: 'Issue', args: { number: 'Int!' } },
      issues: { type: 'IssueConnection!', args: { first: 'Int', after: 'String', states: '[IssueState!]', orderBy: 'IssueOrder' } },
      pullRequest: { type: 'PullRequest', args: { number: 'Int!' } },
      pullRequests: { type: 'PullRequestConnection!', args: { first: 'Int', after: 'String', states: '[PullRequestState!]', orderBy: 'PullRequestOrder' } },
      url: { type: 'String!' },
      description: { type: 'String' },
      private: { type: 'Boolean!' },
    },
  },
  Ref: {
    kind: 'OBJECT',
    fields: {
      name: { type: 'String!' },
      prefix: { type: 'String!' },
    },
  },
  Issue: {
    kind: 'OBJECT',
    fields: {
      id: { type: 'ID!' },
      number: { type: 'Int!' },
      title: { type: 'String!' },
      state: { type: 'IssueState!' },
      body: { type: 'String' },
      url: { type: 'String!' },
      author: { type: 'User' },
      labels: { type: 'LabelConnection', args: { first: 'Int' } },
      comments: { type: 'IssueCommentConnection', args: { first: 'Int' } },
    },
  },
  PullRequest: {
    kind: 'OBJECT',
    fields: {
      id: { type: 'ID!' },
      number: { type: 'Int!' },
      title: { type: 'String!' },
      state: { type: 'PullRequestState!' },
      headRefName: { type: 'String!' },
      baseRefName: { type: 'String!' },
      mergeable: { type: 'MergeableState!' },
      merged: { type: 'Boolean!' },
      files: { type: 'PullRequestFileConnection', args: { first: 'Int' } },
      body: { type: 'String' },
      url: { type: 'String!' },
    },
  },
  IssueConnection: {
    kind: 'OBJECT',
    fields: {
      nodes: { type: '[Issue]' },
      edges: { type: '[IssueEdge]' },
      pageInfo: { type: 'PageInfo!' },
      totalCount: { type: 'Int!' },
    },
  },
  IssueEdge: {
    kind: 'OBJECT',
    fields: {
      node: { type: 'Issue' },
      cursor: { type: 'String!' },
    },
  },
  PullRequestConnection: {
    kind: 'OBJECT',
    fields: {
      nodes: { type: '[PullRequest]' },
      edges: { type: '[PullRequestEdge]' },
      pageInfo: { type: 'PageInfo!' },
      totalCount: { type: 'Int!' },
    },
  },
  PullRequestEdge: {
    kind: 'OBJECT',
    fields: {
      node: { type: 'PullRequest' },
      cursor: { type: 'String!' },
    },
  },
  PullRequestFileConnection: {
    kind: 'OBJECT',
    fields: {
      nodes: { type: '[PullRequestFile]' },
      edges: { type: '[PullRequestFileEdge]' },
      pageInfo: { type: 'PageInfo!' },
      totalCount: { type: 'Int!' },
    },
  },
  PullRequestFileEdge: {
    kind: 'OBJECT',
    fields: {
      node: { type: 'PullRequestFile' },
      cursor: { type: 'String!' },
    },
  },
  PullRequestFile: {
    kind: 'OBJECT',
    fields: {
      path: { type: 'String!' },
      additions: { type: 'Int!' },
      deletions: { type: 'Int!' },
      changeType: { type: 'String!' },
    },
  },
  PageInfo: {
    kind: 'OBJECT',
    fields: {
      hasNextPage: { type: 'Boolean!' },
      hasPreviousPage: { type: 'Boolean!' },
      endCursor: { type: 'String' },
      startCursor: { type: 'String' },
    },
  },
  LabelConnection: {
    kind: 'OBJECT',
    fields: {
      nodes: { type: '[Label]' },
      totalCount: { type: 'Int!' },
    },
  },
  Label: {
    kind: 'OBJECT',
    fields: {
      name: { type: 'String!' },
      color: { type: 'String!' },
    },
  },
  IssueCommentConnection: {
    kind: 'OBJECT',
    fields: {
      nodes: { type: '[IssueComment]' },
      totalCount: { type: 'Int!' },
    },
  },
  IssueComment: {
    kind: 'OBJECT',
    fields: {
      id: { type: 'ID!' },
      body: { type: 'String!' },
      author: { type: 'User' },
    },
  },
  CreateIssuePayload: { kind: 'OBJECT', fields: { issue: { type: 'Issue' } } },
  CloseIssuePayload: { kind: 'OBJECT', fields: { issue: { type: 'Issue' } } },
  CreatePullRequestPayload: { kind: 'OBJECT', fields: { pullRequest: { type: 'PullRequest' } } },
  MergePullRequestPayload: { kind: 'OBJECT', fields: { pullRequest: { type: 'PullRequest' } } },
  AddCommentPayload: { kind: 'OBJECT', fields: { commentEdge: { type: 'CommentEdge' } } },
  CommentEdge: {
    kind: 'OBJECT',
    fields: {
      node: { type: 'IssueComment' },
      cursor: { type: 'String!' },
    },
  },
  IssueState: { kind: 'ENUM', enumValues: ['OPEN', 'CLOSED'] },
  PullRequestState: { kind: 'ENUM', enumValues: ['OPEN', 'CLOSED', 'MERGED'] },
  OrderDirection: { kind: 'ENUM', enumValues: ['ASC', 'DESC'] },
  IssueOrderField: { kind: 'ENUM', enumValues: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'] },
  PullRequestOrderField: { kind: 'ENUM', enumValues: ['CREATED_AT', 'UPDATED_AT'] },
  MergeableState: { kind: 'ENUM', enumValues: ['CONFLICTING', 'MERGEABLE', 'UNKNOWN'] },
  RepositoryPermission: { kind: 'ENUM', enumValues: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'] },
  SearchType: { kind: 'ENUM', enumValues: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT', 'CODE'] },
  PullRequestMergeMethod: { kind: 'ENUM', enumValues: ['MERGE', 'SQUASH', 'REBASE'] },
  IssueOrder: { kind: 'INPUT_OBJECT', inputFields: { field: 'IssueOrderField!', direction: 'OrderDirection!' } },
  PullRequestOrder: { kind: 'INPUT_OBJECT', inputFields: { field: 'PullRequestOrderField!', direction: 'OrderDirection!' } },
  CreateIssueInput: { kind: 'INPUT_OBJECT', inputFields: { repositoryId: 'ID!', title: 'String!', body: 'String' } },
  CloseIssueInput: { kind: 'INPUT_OBJECT', inputFields: { issueId: 'ID!' } },
  CreatePullRequestInput: { kind: 'INPUT_OBJECT', inputFields: { repositoryId: 'ID!', baseRefName: 'String!', headRefName: 'String!', title: 'String!', body: 'String' } },
  MergePullRequestInput: { kind: 'INPUT_OBJECT', inputFields: { pullRequestId: 'ID!', mergeMethod: 'PullRequestMergeMethod' } },
  AddCommentInput: { kind: 'INPUT_OBJECT', inputFields: { subjectId: 'ID!', body: 'String!' } },
  __Schema: {
    kind: 'OBJECT',
    fields: {
      queryType: { type: '__Type!' },
      mutationType: { type: '__Type' },
      types: { type: '[__Type!]!' },
      description: { type: 'String' },
    },
  },
  __Type: {
    kind: 'OBJECT',
    fields: {
      name: { type: 'String' },
      kind: { type: '__TypeKind!' },
      description: { type: 'String' },
      fields: { type: '[__Field!]', args: { includeDeprecated: 'Boolean' } },
      enumValues: { type: '[__EnumValue!]', args: { includeDeprecated: 'Boolean' } },
      inputFields: { type: '[__InputValue!]' },
      interfaces: { type: '[__Type!]' },
      possibleTypes: { type: '[__Type!]' },
      ofType: { type: '__Type' },
    },
  },
  __Field: {
    kind: 'OBJECT',
    fields: {
      name: { type: 'String!' },
      description: { type: 'String' },
      args: { type: '[__InputValue!]!' },
      type: { type: '__Type!' },
    },
  },
  __InputValue: {
    kind: 'OBJECT',
    fields: {
      name: { type: 'String!' },
      description: { type: 'String' },
      type: { type: '__Type!' },
      defaultValue: { type: 'String' },
    },
  },
  __EnumValue: {
    kind: 'OBJECT',
    fields: {
      name: { type: 'String!' },
      description: { type: 'String' },
      isDeprecated: { type: 'Boolean!' },
      deprecationReason: { type: 'String' },
    },
  },
  __TypeKind: { kind: 'ENUM', enumValues: ['SCALAR', 'OBJECT', 'INTERFACE', 'UNION', 'ENUM', 'INPUT_OBJECT', 'LIST', 'NON_NULL'] },
};

/** Parse a type string ('String!', '[IssueState!]', 'Repository') into a ref. */
type TypeRef =
  | { kind: 'NAMED'; name: string }
  | { kind: 'LIST'; ofType: TypeRef }
  | { kind: 'NON_NULL'; ofType: TypeRef };

function parseTypeRef(text: string): TypeRef {
  if (text.endsWith('!')) return { kind: 'NON_NULL', ofType: parseTypeRef(text.slice(0, -1)) };
  if (text.startsWith('[') && text.endsWith(']')) return { kind: 'LIST', ofType: parseTypeRef(text.slice(1, -1)) };
  return { kind: 'NAMED', name: text };
}

/** The named type a ref ultimately points at (unwrapping LIST/NON_NULL). */
function namedOf(ref: TypeRef): string {
  return ref.kind === 'NAMED' ? ref.name : namedOf(ref.ofType);
}

// ---------------------------------------------------------------------------
// Value model: wrapped store objects carry __typename + a __-prefixed payload.
// ---------------------------------------------------------------------------

type AnyValue = Record<string, unknown>;

function wrapUser(login: string): AnyValue {
  return { __typename: 'User', __login: login };
}
function wrapRepo(repo: Repo): AnyValue {
  return { __typename: 'Repository', __repo: repo };
}
function wrapIssue(repo: Repo, issue: Issue): AnyValue {
  return { __typename: 'Issue', __repo: repo, __issue: issue };
}
function wrapPull(repo: Repo, pull: Pull): AnyValue {
  return { __typename: 'PullRequest', __repo: repo, __pull: pull };
}
function issueGlobalId(repo: Repo, issue: Issue): string {
  return globalId('I', repo.id, issue.number);
}
function pullGlobalId(repo: Repo, pull: Pull): string {
  return globalId('PR', repo.id, pull.number);
}
function repoGlobalId(repo: Repo): string {
  return globalId('R', repo.id);
}

function findRepoById(id: number): Repo | undefined {
  for (const repo of store.repos.values()) if (repo.id === id) return repo;
  return undefined;
}

// ---------------------------------------------------------------------------
// Connections (cursor pagination over an ordered list of wrapped items)
// ---------------------------------------------------------------------------

interface PageInfoValue {
  hasNextPage: boolean;
  hasPreviousPage: boolean;
  endCursor: string | null;
  startCursor: string | null;
}

function cursorForNumber(number: number): string {
  return Buffer.from(`c:${number}`, 'utf8').toString('base64');
}

function numberOfCursor(cursor: string | null | undefined): number | null {
  if (typeof cursor !== 'string' || cursor === '') return null;
  try {
    const text = Buffer.from(cursor, 'base64').toString('utf8');
    const match = text.match(/^c:(\d+)$/);
    return match ? Number.parseInt(match[1], 10) : null;
  } catch {
    return null;
  }
}

/**
 * Slice an ordered list of items (each with a numeric `number`) into a
 * connection page. `after` is a cursor from a previous page of the same list.
 */
function paginateItems<T extends { number: number }>(
  items: T[],
  first: number | null | undefined,
  after: string | null | undefined,
): { page: T[]; pageInfo: PageInfoValue } {
  let start = 0;
  const afterNumber = numberOfCursor(after);
  if (afterNumber !== null) {
    const index = items.findIndex((item) => item.number === afterNumber);
    if (index >= 0) start = index + 1;
  }
  const limit = typeof first === 'number' && first >= 0 ? first : items.length;
  const page = items.slice(start, start + limit);
  return {
    page,
    pageInfo: {
      hasNextPage: start + page.length < items.length,
      hasPreviousPage: start > 0,
      endCursor: page.length ? cursorForNumber(page[page.length - 1].number) : null,
      startCursor: page.length ? cursorForNumber(page[0].number) : null,
    },
  };
}

function makeConnection(typeName: string, nodes: AnyValue[], pageInfo: PageInfoValue, totalCount: number): AnyValue {
  return { __typename: typeName, __nodes: nodes, __pageInfo: pageInfo, __totalCount: totalCount };
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

interface ExecCtx {
  variables: Record<string, unknown>;
  fragments: Map<string, FragmentNode>;
  errors: { message: string }[];
}

function resolveValue(node: ValueNode, variables: Record<string, unknown>): unknown {
  switch (node.kind) {
    case 'variable':
      return variables[node.name] ?? null;
    case 'string':
    case 'int':
    case 'float':
    case 'boolean':
      return node.value;
    case 'null':
      return null;
    case 'enum':
      return node.value;
    case 'list':
      return node.values.map((v) => resolveValue(v, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const field of node.fields) out[field.name] = resolveValue(field.value, variables);
      return out;
    }
  }
}

function resolveArgs(field: FieldNode, ctx: ExecCtx): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const arg of field.args) out[arg.name] = resolveValue(arg.value, ctx.variables);
  return out;
}

function fail(ctx: ExecCtx, message: string): null {
  ctx.errors.push({ message });
  return null;
}

/**
 * Resolve one field of a wrapped value. Returns the raw value (scalar,
 * wrapped object, array of wrapped objects, or null). Unknown fields push an
 * error naming the field and its type.
 */
function resolveField(typeName: string, value: AnyValue, field: FieldNode, ctx: ExecCtx): unknown {
  const def = SCHEMA[typeName];
  if (!def) return fail(ctx, `unknown type ${JSON.stringify(typeName)}`);
  if (field.name === '__typename') return typeName;
  const fieldDef = def.fields?.[field.name];
  if (!fieldDef) {
    return fail(ctx, `Cannot query field ${JSON.stringify(field.name)} on type ${JSON.stringify(typeName)}`);
  }
  const args = resolveArgs(field, ctx);
  return resolveKnownField(typeName, value, field.name, args, ctx);
}

function resolveKnownField(
  typeName: string,
  value: AnyValue,
  name: string,
  args: Record<string, unknown>,
  ctx: ExecCtx,
): unknown {
  // Root fields.
  if (typeName === 'Query') {
    if (name === 'viewer') return wrapUser(VIEWER_LOGIN);
    if (name === 'repository') {
      const owner = args.owner as string;
      const repoName = args.name as string;
      if (owner !== VIEWER_LOGIN) return null;
      const repo = store.getRepo(owner, repoName);
      return repo ? wrapRepo(repo) : null;
    }
    if (name === 'node') {
      const parsed = parseGlobalId(String(args.id ?? ''));
      if (!parsed) return null;
      if (parsed.kind === 'R') {
        const repo = findRepoById(Number(parsed.parts[0]));
        return repo ? wrapRepo(repo) : null;
      }
      if (parsed.kind === 'I') {
        const repo = findRepoById(Number(parsed.parts[0]));
        const issue = repo?.issues.find((i) => i.number === Number(parsed.parts[1]));
        return repo && issue ? wrapIssue(repo, issue) : null;
      }
      if (parsed.kind === 'PR') {
        const repo = findRepoById(Number(parsed.parts[0]));
        const pull = repo?.pulls.find((p) => p.number === Number(parsed.parts[1]));
        return repo && pull ? wrapPull(repo, pull) : null;
      }
      return null;
    }
    if (name === '__schema') return { __typename: '__Schema' };
    if (name === '__type') {
      const typeNameArg = args.name as string;
      if (!SCHEMA[typeNameArg]) return null;
      return { __typename: '__Type', __typeRef: parseTypeRef(typeNameArg) };
    }
    return null;
  }

  if (typeName === 'Mutation') return resolveMutation(name, args, ctx);

  if (typeName === 'User') {
    if (name === 'login') return value.__login as string;
    if (name === 'id') return globalId('U', value.__login as string);
    if (name === 'name') return VIEWER_NAME;
    if (name === 'email') return VIEWER_EMAIL;
    return null;
  }

  if (typeName === 'Repository') {
    const repo = value.__repo as Repo;
    switch (name) {
      case 'id': return repoGlobalId(repo);
      case 'databaseId': return repo.id;
      case 'name': return repo.name;
      case 'nameWithOwner': return `${repo.owner}/${repo.name}`;
      case 'owner': return wrapUser(repo.owner);
      case 'defaultBranchRef': {
        const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
        return tip ? { __typename: 'Ref', __name: repo.defaultBranch } : null;
      }
      case 'viewerPermission': return 'ADMIN';
      case 'hasIssuesEnabled': return true;
      case 'url': return `https://example.invalid/${repo.owner}/${repo.name}`;
      case 'description': return repo.description;
      case 'private': return repo.private;
      case 'issue': {
        const issue = repo.issues.find((i) => i.number === args.number);
        return issue ? wrapIssue(repo, issue) : null;
      }
      case 'issues': {
        let issues = [...repo.issues];
        const states = args.states as string[] | undefined;
        if (Array.isArray(states) && states.length) {
          issues = issues.filter((i) => states.includes(i.state.toUpperCase()));
        }
        const orderBy = args.orderBy as { field?: string; direction?: string } | undefined;
        const direction = orderBy?.direction ?? 'ASC';
        const field = orderBy?.field ?? 'CREATED_AT';
        if (field === 'CREATED_AT') {
          issues.sort((a, b) => (direction === 'DESC' ? b.number - a.number : a.number - b.number));
        }
        const { page, pageInfo } = paginateItems(issues, args.first as number | undefined, args.after as string | undefined);
        return makeConnection('IssueConnection', page.map((i) => wrapIssue(repo, i)), pageInfo, issues.length);
      }
      case 'pullRequest': {
        const pull = repo.pulls.find((p) => p.number === args.number);
        return pull ? wrapPull(repo, pull) : null;
      }
      case 'pullRequests': {
        let pulls = [...repo.pulls];
        const states = args.states as string[] | undefined;
        if (Array.isArray(states) && states.length) {
          pulls = pulls.filter((p) => states.includes(pullState(p)));
        }
        const orderBy = args.orderBy as { field?: string; direction?: string } | undefined;
        const direction = orderBy?.direction ?? 'ASC';
        pulls.sort((a, b) => (direction === 'DESC' ? b.number - a.number : a.number - b.number));
        const { page, pageInfo } = paginateItems(pulls, args.first as number | undefined, args.after as string | undefined);
        return makeConnection('PullRequestConnection', page.map((p) => wrapPull(repo, p)), pageInfo, pulls.length);
      }
      default: return null;
    }
  }

  if (typeName === 'Ref') {
    if (name === 'name') return value.__name as string;
    if (name === 'prefix') return 'refs/heads/';
    return null;
  }

  if (typeName === 'Issue') {
    const repo = value.__repo as Repo;
    const issue = value.__issue as Issue;
    switch (name) {
      case 'id': return issueGlobalId(repo, issue);
      case 'number': return issue.number;
      case 'title': return issue.title;
      case 'state': return issue.state.toUpperCase();
      case 'body': return issue.body;
      case 'url': return `https://example.invalid/${repo.owner}/${repo.name}/issues/${issue.number}`;
      case 'author': return wrapUser(VIEWER_LOGIN);
      case 'labels': {
        const nodes = issue.labels.map((label) => ({ __typename: 'Label', __name: label.name, __color: label.color }));
        return makeConnection('LabelConnection', nodes, { hasNextPage: false, hasPreviousPage: false, endCursor: null, startCursor: null }, nodes.length);
      }
      case 'comments': {
        const nodes = issue.comments.map((comment) => ({ __typename: 'IssueComment', __repo: repo, __comment: comment }));
        return makeConnection('IssueCommentConnection', nodes, { hasNextPage: false, hasPreviousPage: false, endCursor: null, startCursor: null }, nodes.length);
      }
      default: return null;
    }
  }

  if (typeName === 'PullRequest') {
    const repo = value.__repo as Repo;
    const pull = value.__pull as Pull;
    switch (name) {
      case 'id': return pullGlobalId(repo, pull);
      case 'number': return pull.number;
      case 'title': return pull.title;
      case 'state': return pullState(pull);
      case 'headRefName': return pull.headRef;
      case 'baseRefName': return pull.baseRef;
      case 'mergeable': return mergeConflict(repo, pull) ? 'CONFLICTING' : 'MERGEABLE';
      case 'merged': return pull.merged;
      case 'body': return pull.body;
      case 'url': return `https://example.invalid/${repo.owner}/${repo.name}/pull/${pull.number}`;
      case 'files': {
        const files = pullFiles(repo, pull);
        const nodes = files.map((file, index) => ({
          __typename: 'PullRequestFile',
          __path: file.filename as string,
          __additions: file.additions as number,
          __deletions: file.deletions as number,
          __changeType: file.status as string,
          number: index + 1,
        }));
        const { page, pageInfo } = paginateItems(nodes, args.first as number | undefined, undefined);
        return makeConnection('PullRequestFileConnection', page, pageInfo, nodes.length);
      }
      default: return null;
    }
  }

  if (typeName.endsWith('Connection')) {
    if (name === 'nodes') return value.__nodes as AnyValue[];
    if (name === 'totalCount') return value.__totalCount as number;
    if (name === 'pageInfo') return { __typename: 'PageInfo', ...(value.__pageInfo as PageInfoValue) };
    if (name === 'edges') {
      const nodes = value.__nodes as AnyValue[];
      const edgeType = typeName.replace('Connection', 'Edge');
      return nodes.map((node) => ({
        __typename: edgeType,
        __node: node,
        __cursor: cursorForNumber((node as { number?: number }).number ?? 0),
      }));
    }
    return null;
  }

  if (typeName.endsWith('Edge')) {
    if (name === 'node') return value.__node as AnyValue;
    if (name === 'cursor') return value.__cursor as string;
    return null;
  }

  if (typeName === 'PageInfo') {
    if (name === 'hasNextPage') return value.hasNextPage as boolean;
    if (name === 'hasPreviousPage') return value.hasPreviousPage as boolean;
    if (name === 'endCursor') return value.endCursor as string | null;
    if (name === 'startCursor') return value.startCursor as string | null;
    return null;
  }

  if (typeName === 'Label') {
    if (name === 'name') return value.__name as string;
    if (name === 'color') return value.__color as string;
    return null;
  }

  if (typeName === 'IssueComment') {
    const comment = value.__comment as Issue['comments'][number];
    if (name === 'id') return globalId('C', comment.id);
    if (name === 'body') return comment.body;
    if (name === 'author') return wrapUser(comment.userLogin);
    return null;
  }

  if (typeName === 'PullRequestFile') {
    if (name === 'path') return value.__path as string;
    if (name === 'additions') return value.__additions as number;
    if (name === 'deletions') return value.__deletions as number;
    if (name === 'changeType') return (value.__changeType as string).toUpperCase();
    return null;
  }

  if (typeName === 'CreateIssuePayload' || typeName === 'CloseIssuePayload') {
    if (name === 'issue') return value.__issue as AnyValue;
    return null;
  }
  if (typeName === 'CreatePullRequestPayload' || typeName === 'MergePullRequestPayload') {
    if (name === 'pullRequest') return value.__pull as AnyValue;
    return null;
  }
  if (typeName === 'AddCommentPayload') {
    if (name === 'commentEdge') return value.__commentEdge as AnyValue;
    return null;
  }
  if (typeName === 'CommentEdge') {
    if (name === 'node') return value.__node as AnyValue;
    if (name === 'cursor') return value.__cursor as string;
    return null;
  }

  if (typeName === '__Schema') {
    if (name === 'queryType') return { __typename: '__Type', __typeRef: parseTypeRef('Query') };
    if (name === 'mutationType') return { __typename: '__Type', __typeRef: parseTypeRef('Mutation') };
    if (name === 'types') {
      return Object.keys(SCHEMA).map((typeNameArg) => ({ __typename: '__Type', __typeRef: parseTypeRef(typeNameArg) }));
    }
    if (name === 'description') return null;
    return null;
  }

  if (typeName === '__Type') {
    return resolveIntrospectionType(value.__typeRef as TypeRef, name, args);
  }

  if (typeName === '__Field') {
    if (name === 'name') return value.__fieldName as string;
    if (name === 'description') return null;
    if (name === 'type') return { __typename: '__Type', __typeRef: parseTypeRef(value.__fieldType as string) };
    if (name === 'args') {
      const fieldArgs = value.__fieldArgs as Record<string, string>;
      return Object.entries(fieldArgs).map(([argName, argType]) => ({
        __typename: '__InputValue',
        __inputName: argName,
        __inputType: argType,
      }));
    }
    return null;
  }

  if (typeName === '__InputValue') {
    if (name === 'name') return value.__inputName as string;
    if (name === 'description') return null;
    if (name === 'type') return { __typename: '__Type', __typeRef: parseTypeRef(value.__inputType as string) };
    if (name === 'defaultValue') return null;
    return null;
  }

  if (typeName === '__EnumValue') {
    if (name === 'name') return value.__enumName as string;
    if (name === 'description') return null;
    if (name === 'isDeprecated') return false;
    if (name === 'deprecationReason') return null;
    return null;
  }

  return null;
}

/** GraphQL state of a PR: MERGED when merged, else CLOSED/OPEN from the REST state. */
function pullState(pull: Pull): string {
  if (pull.merged) return 'MERGED';
  return pull.state === 'closed' ? 'CLOSED' : 'OPEN';
}

function resolveIntrospectionType(ref: TypeRef, name: string, args: Record<string, unknown>): unknown {
  if (name === 'kind') {
    if (ref.kind === 'LIST') return 'LIST';
    if (ref.kind === 'NON_NULL') return 'NON_NULL';
    return SCHEMA[ref.name]?.kind ?? 'SCALAR';
  }
  if (name === 'name') return ref.kind === 'NAMED' ? ref.name : null;
  if (name === 'description') return null;
  if (name === 'ofType') {
    return ref.kind === 'NAMED' ? null : { __typename: '__Type', __typeRef: ref.ofType };
  }
  if (name === 'fields') {
    if (ref.kind !== 'NAMED') return null;
    const def = SCHEMA[ref.name];
    if (!def?.fields) return null;
    return Object.entries(def.fields).map(([fieldName, fieldDef]) => ({
      __typename: '__Field',
      __fieldName: fieldName,
      __fieldType: fieldDef.type,
      __fieldArgs: fieldDef.args ?? {},
    }));
  }
  if (name === 'enumValues') {
    if (ref.kind !== 'NAMED') return null;
    const def = SCHEMA[ref.name];
    if (!def?.enumValues) return null;
    return def.enumValues.map((enumName) => ({ __typename: '__EnumValue', __enumName: enumName }));
  }
  if (name === 'inputFields') {
    if (ref.kind !== 'NAMED') return null;
    const def = SCHEMA[ref.name];
    if (!def?.inputFields) return null;
    return Object.entries(def.inputFields).map(([inputName, inputType]) => ({
      __typename: '__InputValue',
      __inputName: inputName,
      __inputType: inputType,
    }));
  }
  if (name === 'interfaces') return [];
  if (name === 'possibleTypes') return null;
  return null;
}

// ---------------------------------------------------------------------------
// Mutations
// ---------------------------------------------------------------------------

function resolveMutation(name: string, args: Record<string, unknown>, ctx: ExecCtx): unknown {
  const input = (args.input ?? {}) as Record<string, unknown>;
  if (name === 'createIssue') {
    const parsed = parseGlobalId(String(input.repositoryId ?? ''));
    const repo = parsed?.kind === 'R' ? findRepoById(Number(parsed.parts[0])) : undefined;
    if (!repo) return fail(ctx, 'repository not found');
    const issue: Issue = {
      id: nextGlobalId(),
      number: repo.nextNumber,
      title: String(input.title ?? ''),
      body: typeof input.body === 'string' ? input.body : '',
      state: 'open',
      labels: [],
      comments: [],
      createdSeq: nextSeq(),
      cachedJson: null,
    };
    repo.nextNumber += 1;
    repo.issues.push(issue);
    return { __typename: 'CreateIssuePayload', __issue: wrapIssue(repo, issue) };
  }
  if (name === 'closeIssue') {
    const parsed = parseGlobalId(String(input.issueId ?? ''));
    if (parsed?.kind !== 'I') return fail(ctx, 'issue not found');
    const repo = findRepoById(Number(parsed.parts[0]));
    const issue = repo?.issues.find((i) => i.number === Number(parsed.parts[1]));
    if (!repo || !issue) return fail(ctx, 'issue not found');
    issue.state = 'closed';
    issue.cachedJson = null;
    return { __typename: 'CloseIssuePayload', __issue: wrapIssue(repo, issue) };
  }
  if (name === 'createPullRequest') {
    const parsed = parseGlobalId(String(input.repositoryId ?? ''));
    const repo = parsed?.kind === 'R' ? findRepoById(Number(parsed.parts[0])) : undefined;
    if (!repo) return fail(ctx, 'repository not found');
    const headRef = String(input.headRefName ?? '');
    const baseRef = String(input.baseRefName ?? '');
    const headSha = repo.refs.get(`refs/heads/${headRef}`);
    const baseSha = repo.refs.get(`refs/heads/${baseRef}`);
    if (!headSha || !baseSha) return fail(ctx, 'head or base branch does not exist');
    const duplicate = repo.pulls.find((p) => p.state === 'open' && p.headRef === headRef && p.baseRef === baseRef);
    if (duplicate) return fail(ctx, 'a pull request already exists for these branches');
    const pull: Pull = {
      id: nextGlobalId(),
      number: repo.nextNumber,
      title: String(input.title ?? ''),
      body: typeof input.body === 'string' ? input.body : '',
      state: 'open',
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      headRef: headRef,
      baseRef: baseRef,
      headSha,
      baseSha,
      createdSeq: nextSeq(),
    };
    repo.nextNumber += 1;
    repo.pulls.push(pull);
    return { __typename: 'CreatePullRequestPayload', __pull: wrapPull(repo, pull) };
  }
  if (name === 'mergePullRequest') {
    const parsed = parseGlobalId(String(input.pullRequestId ?? ''));
    if (parsed?.kind !== 'PR') return fail(ctx, 'pull request not found');
    const repo = findRepoById(Number(parsed.parts[0]));
    const pull = repo?.pulls.find((p) => p.number === Number(parsed.parts[1]));
    if (!repo || !pull) return fail(ctx, 'pull request not found');
    const result = squashMerge(repo, pull);
    if (!result.ok) return fail(ctx, result.reason ?? 'merge failed');
    return { __typename: 'MergePullRequestPayload', __pull: wrapPull(repo, pull) };
  }
  if (name === 'addComment') {
    const parsed = parseGlobalId(String(input.subjectId ?? ''));
    if (parsed?.kind !== 'I') return fail(ctx, 'issue not found');
    const repo = findRepoById(Number(parsed.parts[0]));
    const issue = repo?.issues.find((i) => i.number === Number(parsed.parts[1]));
    if (!repo || !issue) return fail(ctx, 'issue not found');
    const comment = {
      id: nextGlobalId(),
      body: String(input.body ?? ''),
      userLogin: VIEWER_LOGIN,
      createdSeq: nextSeq(),
    };
    issue.comments.push(comment);
    issue.cachedJson = null;
    const node = { __typename: 'IssueComment', __repo: repo, __comment: comment };
    return {
      __typename: 'AddCommentPayload',
      __commentEdge: { __typename: 'CommentEdge', __node: node, __cursor: cursorForNumber(comment.id) },
    };
  }
  return fail(ctx, `unknown mutation ${JSON.stringify(name)}`);
}

// ---------------------------------------------------------------------------
// Selection resolution
// ---------------------------------------------------------------------------

function resolveSelection(
  typeName: string,
  value: AnyValue,
  selection: SelectionNode[],
  ctx: ExecCtx,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const node of selection) {
    if (node.kind === 'fragmentSpread') {
      const fragment = ctx.fragments.get(node.name);
      if (!fragment) {
        ctx.errors.push({ message: `unknown fragment ${JSON.stringify(node.name)}` });
        continue;
      }
      if (fragment.typeCondition !== typeName) continue;
      Object.assign(out, resolveSelection(typeName, value, fragment.selection, ctx));
      continue;
    }
    if (node.kind === 'inlineFragment') {
      if (node.typeCondition && node.typeCondition !== typeName) continue;
      Object.assign(out, resolveSelection(typeName, value, node.selection, ctx));
      continue;
    }
    const field = node.field;
    const key = field.alias ?? field.name;
    const raw = resolveField(typeName, value, field, ctx);
    if (ctx.errors.length) return out;
    if (raw === null || raw === undefined) {
      out[key] = null;
      continue;
    }
    if (!field.selection) {
      out[key] = raw;
      continue;
    }
    if (Array.isArray(raw)) {
      out[key] = raw.map((item) => {
        if (item === null || item === undefined) return null;
        const itemType = (item as AnyValue).__typename as string;
        return resolveSelection(itemType, item as AnyValue, field.selection!, ctx);
      });
      continue;
    }
    if (typeof raw === 'object') {
      const itemType = (raw as AnyValue).__typename as string;
      out[key] = resolveSelection(itemType, raw as AnyValue, field.selection, ctx);
      continue;
    }
    out[key] = raw;
  }
  return out;
}

function executeOperation(
  doc: Document,
  operation: OperationNode,
  variables: Record<string, unknown>,
): { data: Record<string, unknown> | null; errors?: { message: string }[] } {
  const merged: Record<string, unknown> = { ...variables };
  for (const varDef of operation.variables) {
    if (merged[varDef.name] === undefined && varDef.defaultValue) {
      merged[varDef.name] = resolveValue(varDef.defaultValue, merged);
    }
  }
  const ctx: ExecCtx = { variables: merged, fragments: doc.fragments, errors: [] };
  const rootType = operation.kind === 'mutation' ? 'Mutation' : 'Query';
  const data = resolveSelection(rootType, { __typename: rootType }, operation.selection, ctx);
  if (ctx.errors.length) return { data: null, errors: ctx.errors };
  return { data };
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

export function handleGraphQL(ctx: Ctx): boolean {
  if (ctx.pathname !== '/api/graphql') return false;
  if (ctx.method !== 'POST') {
    sendJson(ctx, 404, { message: 'Not Found' });
    return true;
  }
  const body = parseJsonBody(ctx);
  if (body === null) {
    sendJson(ctx, 400, { message: 'malformed JSON body' });
    return true;
  }
  const runOne = (entry: unknown): Record<string, unknown> => {
    if (!isPlainObject(entry) || typeof entry.query !== 'string') {
      return { data: null, errors: [{ message: 'query is required' }] };
    }
    let doc: Document;
    try {
      doc = parseDocument(entry.query);
    } catch (err) {
      return { data: null, errors: [{ message: err instanceof Error ? err.message : 'parse error' }] };
    }
    const variables = isPlainObject(entry.variables) ? (entry.variables as Record<string, unknown>) : {};
    const operationName = typeof entry.operationName === 'string' ? entry.operationName : null;
    const operation =
      doc.operations.find((op) => op.name === operationName) ??
      (doc.operations.length === 1 ? doc.operations[0] : undefined);
    if (!operation) {
      return { data: null, errors: [{ message: 'operation not found' }] };
    }
    return executeOperation(doc, operation, variables);
  };
  if (Array.isArray(body)) {
    sendJson(ctx, 200, body.map(runOne));
    return true;
  }
  sendJson(ctx, 200, runOne(body));
  return true;
}
