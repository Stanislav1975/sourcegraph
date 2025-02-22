import { AdjustmentDirection, DiffPart, PositionAdjuster } from '@sourcegraph/codeintellify'
import { trimStart } from 'lodash'
import { map } from 'rxjs/operators'
import { Omit } from 'utility-types'
import { PlatformContext } from '../../../../shared/src/platform/context'
import {
    FileSpec,
    PositionSpec,
    RawRepoSpec,
    RepoSpec,
    ResolvedRevSpec,
    RevSpec,
    ViewStateSpec,
} from '../../../../shared/src/util/url'
import { fetchBlobContentLines } from '../../shared/repo/backend'
import { querySelectorOrSelf } from '../../shared/util/dom'
import { toAbsoluteBlobURL } from '../../shared/util/url'
import { CodeHost, MountGetter } from '../code_intelligence'
import { CodeView, toCodeViewResolver } from '../code_intelligence/code_views'
import { NativeTooltip } from '../code_intelligence/native_tooltips'
import { getSelectionsFromHash, observeSelectionsFromHash } from '../code_intelligence/util/selections'
import { ViewResolver } from '../code_intelligence/views'
import { markdownBodyViewResolver } from './content_views'
import { diffDomFunctions, searchCodeSnippetDOMFunctions, singleFileDOMFunctions } from './dom_functions'
import { getCommandPaletteMount } from './extensions'
import { resolveDiffFileInfo, resolveFileInfo, resolveSnippetFileInfo } from './file_info'
import { commentTextFieldResolver } from './text_fields'
import { setElementTooltip } from './tooltip'
import { getFileContainers, parseURL } from './util'

/**
 * Creates the mount element for the CodeViewToolbar on code views containing
 * a `.file-actions` element, for instance:
 * - A diff code view on a PR's files page, or a commit page
 * - An older GHE single file code view (newer GitHub.com code views use createFileLineContainerToolbarMount)
 */
export function createFileActionsToolbarMount(codeView: HTMLElement): HTMLElement {
    const className = 'github-file-actions-toolbar-mount'
    const existingMount = codeView.querySelector('.' + className) as HTMLElement
    if (existingMount) {
        return existingMount
    }

    const mountEl = document.createElement('div')
    mountEl.className = className

    const fileActions = codeView.querySelector('.file-actions')
    if (!fileActions) {
        throw new Error('Could not find GitHub file actions with selector .file-actions')
    }

    // Old GitHub Enterprise PR views have a "☑ show comments" text that we want to insert *after*
    const showCommentsElement = codeView.querySelector('.show-file-notes')
    if (showCommentsElement) {
        showCommentsElement.insertAdjacentElement('afterend', mountEl)
    } else {
        fileActions.prepend(mountEl)
    }

    return mountEl
}

const toolbarButtonProps = {
    className: 'btn btn-sm tooltipped tooltipped-s',
}

const diffCodeView: Omit<CodeView, 'element'> = {
    dom: diffDomFunctions,
    getToolbarMount: createFileActionsToolbarMount,
    resolveFileInfo: resolveDiffFileInfo,
    toolbarButtonProps,
}

const diffConversationCodeView: Omit<CodeView, 'element'> = {
    ...diffCodeView,
    getToolbarMount: undefined,
}

const singleFileCodeView: Omit<CodeView, 'element'> = {
    dom: singleFileDOMFunctions,
    getToolbarMount: createFileActionsToolbarMount,
    resolveFileInfo,
    toolbarButtonProps,
    getSelections: getSelectionsFromHash,
    observeSelections: observeSelectionsFromHash,
}

/**
 * Some code snippets get leading white space trimmed. This adjusts based on
 * this. See an example here https://github.com/sourcegraph/browser-extensions/issues/188.
 */
const getSnippetPositionAdjuster = (
    requestGraphQL: PlatformContext['requestGraphQL']
): PositionAdjuster<RepoSpec & RevSpec & FileSpec & ResolvedRevSpec> => ({ direction, codeView, position }) =>
    fetchBlobContentLines({ ...position, requestGraphQL }).pipe(
        map(lines => {
            const codeElement = singleFileDOMFunctions.getCodeElementFromLineNumber(
                codeView,
                position.line,
                position.part
            )
            if (!codeElement) {
                throw new Error('(adjustPosition) could not find code element for line provided')
            }

            const actualLine = lines[position.line - 1]
            const documentLine = codeElement.textContent || ''

            const actualLeadingWhiteSpace = actualLine.length - trimStart(actualLine).length
            const documentLeadingWhiteSpace = documentLine.length - trimStart(documentLine).length

            const modifier = direction === AdjustmentDirection.ActualToCodeView ? -1 : 1
            const delta = Math.abs(actualLeadingWhiteSpace - documentLeadingWhiteSpace) * modifier

            return {
                line: position.line,
                character: position.character + delta,
            }
        })
    )

const searchResultCodeViewResolver = toCodeViewResolver('.code-list-item', {
    dom: searchCodeSnippetDOMFunctions,
    getPositionAdjuster: getSnippetPositionAdjuster,
    resolveFileInfo: resolveSnippetFileInfo,
    toolbarButtonProps,
})

const snippetCodeView: Omit<CodeView, 'element'> = {
    dom: singleFileDOMFunctions,
    resolveFileInfo: resolveSnippetFileInfo,
    getPositionAdjuster: getSnippetPositionAdjuster,
}

export const createFileLineContainerToolbarMount: NonNullable<CodeView['getToolbarMount']> = (
    codeViewElement: HTMLElement
): HTMLElement => {
    const className = 'sourcegraph-app-annotator'
    const existingMount = codeViewElement.querySelector(`.${className}`) as HTMLElement
    if (existingMount) {
        return existingMount
    }
    const mountEl = document.createElement('div')
    mountEl.style.display = 'inline-flex'
    mountEl.style.verticalAlign = 'middle'
    mountEl.style.alignItems = 'center'
    mountEl.className = className
    const rawURLLink = codeViewElement.querySelector('#raw-url')
    const buttonGroup = rawURLLink && rawURLLink.closest('.BtnGroup')
    if (!buttonGroup || !buttonGroup.parentNode) {
        throw new Error('File actions not found')
    }
    buttonGroup.parentNode.insertBefore(mountEl, buttonGroup)
    return mountEl
}

/**
 * Matches the modern single-file code view, or snippets embedded in comments.
 *
 */
export const fileLineContainerResolver: ViewResolver<CodeView> = {
    selector: '.js-file-line-container',
    resolveView: (fileLineContainer: HTMLElement): CodeView | null => {
        const embeddedBlobWrapper = fileLineContainer.closest('.blob-wrapper-embedded')
        if (embeddedBlobWrapper) {
            // This is a snippet embedded in a comment.
            // Resolve to `.blob-wrapper-embedded`'s parent element,
            // the smallest element that contains both the code and
            // the HTML anchor allowing to resolve the file info.
            const element = embeddedBlobWrapper.parentElement!
            return {
                element,
                ...snippetCodeView,
            }
        }
        const { pageType } = parseURL()
        if (pageType !== 'blob') {
            // this is not a single-file code view
            return null
        }
        const repositoryContent = fileLineContainer.closest('.repository-content')
        if (!repositoryContent) {
            throw new Error('Could not find repository content element')
        }
        return {
            element: repositoryContent as HTMLElement,
            ...singleFileCodeView,
            getToolbarMount: createFileLineContainerToolbarMount,
        }
    },
}

const genericCodeViewResolver: ViewResolver<CodeView> = {
    selector: '.file',
    resolveView: (elem: HTMLElement): CodeView | null => {
        if (elem.querySelector('article.markdown-body')) {
            // This code view is rendered markdown, we shouldn't add code intelligence
            return null
        }

        // This is a suggested change on a GitHub PR
        if (elem.closest('.js-suggested-changes-blob')) {
            return null
        }

        const { pageType } = parseURL()
        const isSingleCodeFile =
            pageType === 'blob' &&
            document.getElementsByClassName('file').length === 1 &&
            document.getElementsByClassName('diff-view').length === 0

        if (isSingleCodeFile) {
            return { element: elem, ...singleFileCodeView }
        }

        if (elem.closest('.discussion-item-body') || elem.classList.contains('js-comment-container')) {
            // This code view is embedded on a PR conversation page.
            return { element: elem, ...diffConversationCodeView }
        }

        return { element: elem, ...diffCodeView }
    },
}

/**
 * Returns true if the current page is GitHub Enterprise.
 */
export function checkIsGitHubEnterprise(): boolean {
    const ogSiteName = document.head.querySelector<HTMLMetaElement>('meta[property="og:site_name"]')
    return (
        !!ogSiteName &&
        // GitHub Enterprise v2.14.11 has "GitHub" as og:site_name
        (ogSiteName.content === 'GitHub Enterprise' || ogSiteName.content === 'GitHub') &&
        document.body.classList.contains('enterprise')
    )
}

/**
 * Returns true if the current page is github.com.
 */
export const checkIsGitHubDotCom = (): boolean => /^https?:\/\/(www.)?github.com/.test(window.location.href)

/**
 * Returns true if the current page is either github.com or GitHub Enterprise.
 */
export const checkIsGitHub = (): boolean => checkIsGitHubDotCom() || checkIsGitHubEnterprise()

const OPEN_ON_SOURCEGRAPH_ID = 'open-on-sourcegraph'

export const createOpenOnSourcegraphIfNotExists: MountGetter = (container: HTMLElement): HTMLElement | null => {
    const pageheadActions = querySelectorOrSelf(container, '.pagehead-actions')
    // If ran on page that isn't under a repository namespace.
    if (!pageheadActions || pageheadActions.children.length === 0) {
        return null
    }
    // Check for existing
    let mount = pageheadActions.querySelector<HTMLElement>('#' + OPEN_ON_SOURCEGRAPH_ID)
    if (mount) {
        return mount
    }
    // Create new
    mount = document.createElement('li')
    mount.id = OPEN_ON_SOURCEGRAPH_ID
    pageheadActions.insertAdjacentElement('afterbegin', mount)
    return mount
}

const nativeTooltipResolver: ViewResolver<NativeTooltip> = {
    selector: '.js-tagsearch-popover',
    resolveView: element => ({ element }),
}

export const githubCodeHost: CodeHost = {
    type: 'github',
    name: checkIsGitHubEnterprise() ? 'GitHub Enterprise' : 'GitHub',
    codeViewResolvers: [genericCodeViewResolver, fileLineContainerResolver, searchResultCodeViewResolver],
    contentViewResolvers: [markdownBodyViewResolver],
    textFieldResolvers: [commentTextFieldResolver],
    nativeTooltipResolvers: [nativeTooltipResolver],
    getContext: () => {
        const header = document.querySelector('.repohead-details-container')
        const repoHeaderHasPrivateMarker = !!(header && header.querySelector('.private'))
        return {
            ...parseURL(),
            privateRepository: window.location.hostname !== 'github.com' || repoHeaderHasPrivateMarker,
        }
    },
    getViewContextOnSourcegraphMount: createOpenOnSourcegraphIfNotExists,
    viewOnSourcegraphButtonClassProps: {
        className: 'btn btn-sm tooltipped tooltipped-s',
        iconClassName: 'action-item__icon--github v-align-text-bottom',
    },
    check: checkIsGitHub,
    getCommandPaletteMount,
    commandPaletteClassProps: {
        buttonClassName: 'Header-link',
        popoverClassName: 'Box',
        formClassName: 'p-1',
        inputClassName: 'form-control input-sm header-search-input jump-to-field',
        listClassName: 'p-0 m-0 js-navigation-container jump-to-suggestions-results-container',
        selectedListItemClassName: 'navigation-focus',
        listItemClassName:
            'd-flex flex-justify-start flex-items-center p-0 f5 navigation-item js-navigation-item js-jump-to-scoped-search',
        actionItemClassName:
            'command-palette-action-item--github no-underline d-flex flex-auto flex-items-center jump-to-suggestions-path p-2',
        noResultsClassName: 'd-flex flex-auto flex-items-center jump-to-suggestions-path p-2',
    },
    codeViewToolbarClassProps: {
        className: 'code-view-toolbar--github',
        listItemClass: 'code-view-toolbar__item--github BtnGroup',
        actionItemClass: 'btn btn-sm tooltipped tooltipped-s BtnGroup-item action-item--github',
        actionItemPressedClass: 'selected',
        actionItemIconClass: 'action-item__icon--github v-align-text-bottom',
    },
    completionWidgetClassProps: {
        widgetClassName: 'suggester-container',
        widgetContainerClassName: 'suggester',
        listClassName: 'suggestions',
        selectedListItemClassName: 'navigation-focus',
        listItemClassName: 'text-normal',
    },
    hoverOverlayClassProps: {
        className: 'Box',
        actionItemClassName: 'btn btn-secondary',
        actionItemPressedClassName: 'active',
        closeButtonClassName: 'btn',
        alertClassName: 'alert-info',
    },
    setElementTooltip,
    linkPreviewContentClass: 'text-small text-gray p-1 mx-1 border rounded-1 bg-gray text-gray-dark',
    urlToFile: (
        sourcegraphURL: string,
        location: Partial<RepoSpec> &
            RawRepoSpec &
            RevSpec &
            FileSpec &
            Partial<PositionSpec> &
            Partial<ViewStateSpec> & { part?: DiffPart }
    ) => {
        if (location.viewState) {
            // A view state means that a panel must be shown, and panels are currently only supported on
            // Sourcegraph (not code hosts).
            return toAbsoluteBlobURL(sourcegraphURL, {
                ...location,
                repoName: location.repoName || location.rawRepoName,
            })
        }

        // Make sure the location is also on this github instance, return an absolute URL otherwise.
        const sameCodeHost = location.rawRepoName.startsWith(window.location.hostname)
        if (!sameCodeHost) {
            return toAbsoluteBlobURL(sourcegraphURL, {
                ...location,
                repoName: location.repoName || location.rawRepoName,
            })
        }

        const rev = location.rev || 'HEAD'
        // If we're provided options, we can make the j2d URL more specific.
        const { rawRepoName } = parseURL()

        const sameRepo = rawRepoName === location.rawRepoName
        // Stay on same page in PR if possible.
        if (sameRepo && location.part) {
            const containers = getFileContainers()
            for (const container of containers) {
                const header = container.querySelector('.file-header') as HTMLElement
                const anchorPath = header.dataset.path
                if (anchorPath === location.filePath) {
                    const anchorUrl = header.dataset.anchor
                    const url = `${window.location.origin}${window.location.pathname}#${anchorUrl}${
                        location.part === 'base' ? 'L' : 'R'
                    }${location.position ? location.position.line : ''}`

                    return url
                }
            }
        }

        const fragment = location.position
            ? `#L${location.position.line}${location.position.character ? ':' + location.position.character : ''}`
            : ''
        return `https://${location.rawRepoName}/blob/${rev}/${location.filePath}${fragment}`
    },
    codeViewsRequireTokenization: true,
}
