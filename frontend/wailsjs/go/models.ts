export namespace downloader {
	
	export class Post {
	    platform: string;
	    id: string;
	    title: string;
	    author: string;
	    dir: string;
	    files: string[];
	    count: number;
	    audioUrl: string;
	    audioName: string;
	    audioCandidates?: string[];
	
	    static createFrom(source: any = {}) {
	        return new Post(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.platform = source["platform"];
	        this.id = source["id"];
	        this.title = source["title"];
	        this.author = source["author"];
	        this.dir = source["dir"];
	        this.files = source["files"];
	        this.count = source["count"];
	        this.audioUrl = source["audioUrl"];
	        this.audioName = source["audioName"];
	        this.audioCandidates = source["audioCandidates"];
	    }
	}

}

export namespace inpaint {
	
	export class TaskParams {
	    Boxes: number[][];
	    Relative: boolean;
	    Dilate: number;
	    Margin: number;
	    MaskPath: string;
	    Strategy: string;
	
	    static createFrom(source: any = {}) {
	        return new TaskParams(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Boxes = source["Boxes"];
	        this.Relative = source["Relative"];
	        this.Dilate = source["Dilate"];
	        this.Margin = source["Margin"];
	        this.MaskPath = source["MaskPath"];
	        this.Strategy = source["Strategy"];
	    }
	}

}

export namespace main {
	
	export class CacheClearPayload {
	    freedBytes: number;
	    files: number;
	    kept: number;
	    nothing: boolean;
	
	    static createFrom(source: any = {}) {
	        return new CacheClearPayload(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.freedBytes = source["freedBytes"];
	        this.files = source["files"];
	        this.kept = source["kept"];
	        this.nothing = source["nothing"];
	    }
	}
	export class CacheUsagePayload {
	    totalBytes: number;
	    freeBytes: number;
	    keptBytes: number;
	    keptDir: number;
	
	    static createFrom(source: any = {}) {
	        return new CacheUsagePayload(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.totalBytes = source["totalBytes"];
	        this.freeBytes = source["freeBytes"];
	        this.keptBytes = source["keptBytes"];
	        this.keptDir = source["keptDir"];
	    }
	}
	export class TaskParams {
	    boxes: number[][];
	    relative: boolean;
	    dilate: number;
	    margin: number;
	    maskPath: string;
	    strategy: string;
	
	    static createFrom(source: any = {}) {
	        return new TaskParams(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.boxes = source["boxes"];
	        this.relative = source["relative"];
	        this.dilate = source["dilate"];
	        this.margin = source["margin"];
	        this.maskPath = source["maskPath"];
	        this.strategy = source["strategy"];
	    }
	}
	export class ThumbInfo {
	    width: number;
	    height: number;
	    thumb: string;
	
	    static createFrom(source: any = {}) {
	        return new ThumbInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.width = source["width"];
	        this.height = source["height"];
	        this.thumb = source["thumb"];
	    }
	}
	export class taskAudioPayload {
	    candidates: string[];
	    name: string;
	
	    static createFrom(source: any = {}) {
	        return new taskAudioPayload(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.candidates = source["candidates"];
	        this.name = source["name"];
	    }
	}

}

export namespace queue {
	
	export class TaskProgress {
	    index: number;
	    total: number;
	    name: string;
	    status: string;
	
	    static createFrom(source: any = {}) {
	        return new TaskProgress(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.index = source["index"];
	        this.total = source["total"];
	        this.name = source["name"];
	        this.status = source["status"];
	    }
	}
	export class BatchTask {
	    id: string;
	    type: string;
	    state: string;
	    platform?: string;
	    url?: string;
	    inDir?: string;
	    outDir?: string;
	    params?: inpaint.TaskParams;
	    attempts: number;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    startedAt?: any;
	    // Go type: time
	    finishedAt?: any;
	    err?: string;
	    resultDir?: string;
	    postRef?: string;
	    title?: string;
	    files?: string[];
	    summary?: string;
	    failList?: string[];
	    audioUrl?: string;
	    audioCandidates?: string[];
	    audioName?: string;
	    audioChecked?: boolean;
	    progress?: TaskProgress;
	    // Go type: time
	    nextRetryAt?: any;
	
	    static createFrom(source: any = {}) {
	        return new BatchTask(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.type = source["type"];
	        this.state = source["state"];
	        this.platform = source["platform"];
	        this.url = source["url"];
	        this.inDir = source["inDir"];
	        this.outDir = source["outDir"];
	        this.params = this.convertValues(source["params"], inpaint.TaskParams);
	        this.attempts = source["attempts"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.startedAt = this.convertValues(source["startedAt"], null);
	        this.finishedAt = this.convertValues(source["finishedAt"], null);
	        this.err = source["err"];
	        this.resultDir = source["resultDir"];
	        this.postRef = source["postRef"];
	        this.title = source["title"];
	        this.files = source["files"];
	        this.summary = source["summary"];
	        this.failList = source["failList"];
	        this.audioUrl = source["audioUrl"];
	        this.audioCandidates = source["audioCandidates"];
	        this.audioName = source["audioName"];
	        this.audioChecked = source["audioChecked"];
	        this.progress = this.convertValues(source["progress"], TaskProgress);
	        this.nextRetryAt = this.convertValues(source["nextRetryAt"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class EnqueueFailure {
	    input: string;
	    reason: string;
	
	    static createFrom(source: any = {}) {
	        return new EnqueueFailure(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.input = source["input"];
	        this.reason = source["reason"];
	    }
	}
	export class EnqueueReport {
	    tasks: BatchTask[];
	    failures?: EnqueueFailure[];
	
	    static createFrom(source: any = {}) {
	        return new EnqueueReport(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.tasks = this.convertValues(source["tasks"], BatchTask);
	        this.failures = this.convertValues(source["failures"], EnqueueFailure);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

