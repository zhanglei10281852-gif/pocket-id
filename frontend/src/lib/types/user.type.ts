import type { Locale } from '#lib/paraglide/runtime.js';
import type { CustomClaim } from './custom-claim.type';
import type { UserGroup } from './user-group.type';

export type User = {
	id: string;
	username: string;
	email: string | undefined;
	emailVerified: boolean;
	firstName: string;
	lastName?: string;
	displayName: string;
	isAdmin: boolean;
	userGroups: UserGroup[];
	customClaims: CustomClaim[];
	locale?: Locale;
	ldapId?: string;
	disabled?: boolean;
};

export type UserCreate = Omit<User, 'id' | 'customClaims' | 'ldapId' | 'userGroups'>;

export type AccountUpdate = Omit<UserCreate, 'isAdmin' | 'disabled' | 'emailVerified'>;

export type UserSignUp = Omit<
	UserCreate,
	'isAdmin' | 'disabled' | 'displayName' | 'emailVerified'
> & {
	token?: string;
	// Stable identifier of one registration attempt, reused across retries so the backend never creates a duplicate account
	attemptId?: string;
};
