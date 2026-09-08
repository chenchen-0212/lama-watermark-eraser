export namespace downloader {
	
	export class Post {
	    platform: string;
	    id: string;
	    title: string;
	    author: string;
	    dir: string;
	    files: string[];
	    count: number;
	
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
	    }
	}

}

export namespace main {
	
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

}

